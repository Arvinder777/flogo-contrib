package AzureSendMessage

import (
	"github.com/TIBCOSoftware/flogo-lib/core/activity"
	"github.com/TIBCOSoftware/flogo-lib/logger"
	
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)
import mylog "log"

// THIS IS ADDED
// log is the default package logger which we'll use to log
var log = logger.GetLogger("activity-helloworld")

// MyActivity is a stub for your Activity implementation
type MyActivity struct {
	metadata *activity.Metadata
}

// NewActivity creates a new activity
func NewActivity(metadata *activity.Metadata) activity.Activity {
	return &MyActivity{metadata: metadata}
}

// Metadata implements activity.Activity.Metadata
func (a *MyActivity) Metadata() *activity.Metadata {
	return a.metadata
}

const Rfc2616Time = "Mon, 02 Jan 2006 15:04:05 MST"

const (
	headerBrokerProperties = "BrokerProperties"
	headerContentType      = "Content-Type"
	headerDate             = "Date"
)

type HttpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

var httpClientOverride HttpClient = nil

// Sets the package's http client.
func SetHttpClient(client HttpClient) {
	httpClientOverride = client
}

// Properties represents the key-value pairs of message properties.
type Properties map[string]string

// Get gets the first value associated with the given key.
// It is case insensitive; textproto.CanonicalMIMEHeaderKey is used
// to canonicalize the provided key.
// If there are no values associated with the key, Get returns "".
func (p Properties) Get(key string) string {
	if p == nil {
		return ""
	}
	return p[textproto.CanonicalMIMEHeaderKey(key)]
}

// Set sets the header entries associated with key to
// the single element value. It replaces any existing
// values associated with key.
func (p Properties) Set(key, value string) {
	p[textproto.CanonicalMIMEHeaderKey(key)] = value
}

// Queue Message.
//
// See https://docs.microsoft.com/en-us/rest/api/servicebus/message-headers-and-properties
type Message struct {
	ContentType             string
	CorrelationId           string
	SessionId               string
	DeliveryCount           int
	LockedUntilUtc          time.Time
	LockToken               string
	Id                      string
	Label                   string
	ReplyTo                 string
	EnqueuedTimeUtc         time.Time
	SequenceNumber          int64
	TimeToLive              int
	To                      string
	ScheduledEnqueueTimeUtc time.Time
	ReplyToSessionId        string
	PartitionKey            string

	Properties Properties

	Body []byte
}

func NewMessage(body []byte) *Message {

	return &Message {
		Body: body,
		Properties: Properties{},
	}
}

// Thread-safe client for Azure Service Bus Queue.
type QueueClient struct {
	// Service Bus Namespace e.g. https://<yournamespace>.servicebus.windows.net
	Namespace string

	// Policy name e.g. RootManageSharedAccessKey
	KeyName string

	// Policy value.
	KeyValue string

	// Name of the queue.
	QueueName string

	// Request timeout in seconds.
	Timeout int

	mu         sync.Mutex
	httpClient HttpClient
}

// This operation atomically retrieves and locks a message from a queue or subscription for processing.
// The message is guaranteed not to be delivered to other receivers (on the same queue or subscription only) during the
// lock duration specified in the queue description.
// When the lock expires, the message becomes available to other receivers.
// In order to complete processing of the message, the receiver should issue a delete command with the
// lock ID received from this operation. To abandon processing of the message and unlock it for other receivers,
// an Unlock Message command should be issued, otherwise the lock duration period can expire.

// For more information see https://docs.microsoft.com/en-us/rest/api/servicebus/peek-lock-message-non-destructive-read
func (q *QueueClient) GetMessage() (*Message, error) {

	req, err := q.createRequest("messages/head?timeout="+strconv.Itoa(q.Timeout), "POST")

	if err != nil {
		return nil, wrap(err, "Request create failed")
	}
	resp, err := q.getClient().Do(req)

	if err != nil {
		return nil, wrap(err, "Sending POST createRequest failed")
	}

	defer resp.Body.Close()

	if err := handleStatusCode(resp); err != nil {
		return nil, err
	}

	return parseMessage(resp)
}

// Sends message to a Service Bus queue.
func (q *QueueClient) SendMessage(msg *Message) error {
	req, err := q.createRequestFromMessage("messages/", "POST", msg)

	if err != nil {
		return wrap(err, "Request create failed")
	}

	resp, err := q.getClient().Do(req)

	if err != nil {
		return wrap(err, "Sending POST createRequest failed")
	}

	defer resp.Body.Close()

	return handleStatusCode(resp)
}

// Unlocks a message for processing by other receivers on a specified subscription.
// This operation deletes the lock object, causing the message to be unlocked.
// Before the operation is called, a receiver must first lock the message.
//
// For more information see https://docs.microsoft.com/en-us/rest/api/servicebus/unlock-message
func (q *QueueClient) UnlockMessage(msg *Message) error {
	req, err := q.createRequest("messages/"+msg.Id+"/"+msg.LockToken, "PUT")

	if err != nil {
		return wrap(err, "Request create failed")
	}

	resp, err := q.getClient().Do(req)

	if err != nil {
		return wrap(err, "Sending PUT createRequest failed")
	}

	defer resp.Body.Close()

	return handleStatusCode(resp)
}

// This operation completes the processing of a locked message and deletes it from the queue or subscription.
// This operation should only be called after successfully processing a previously locked message,
// in order to maintain At-Least-Once delivery assurances.
//
// For more information see https://docs.microsoft.com/en-us/rest/api/servicebus/delete-message
func (q *QueueClient) DeleteMessage(msg *Message) error {
	req, err := q.createRequest("messages/"+msg.Id+"/"+msg.LockToken, "DELETE")

	if err != nil {
		return wrap(err, "Request create failed")
	}

	resp, err := q.getClient().Do(req)

	if err != nil {
		return wrap(err, "Sending DELETE createRequest failed")
	}

	defer resp.Body.Close()

	return handleStatusCode(resp)
}

const azureQueueURL = "https://%s.servicebus.windows.net:443/%s/"

func (q *QueueClient) createRequest(path string, method string) (*http.Request, error) {
	url := fmt.Sprintf(azureQueueURL, q.Namespace, q.QueueName) + path

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", q.makeAuthHeader(url, time.Now()))
	return req, nil
}

func (q *QueueClient) createRequestFromMessage(path string, method string, msg *Message) (*http.Request, error) {
	url := fmt.Sprintf(azureQueueURL, q.Namespace, q.QueueName) + path

	req, err := http.NewRequest(method, url, bytes.NewBuffer(msg.Body))
	if err != nil {
		return nil, err
	}

	for k, v := range msg.Properties {
		req.Header.Set(k, v)
	}

	// set BrokeredProperties header
	b := brokerProperties{}
	b.CopyFromMessage(msg)
	bs, err := b.Marshal()
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerBrokerProperties, bs)

	// set Content-Type header
	if msg.ContentType != "" {
		req.Header.Set("Content-Type", msg.ContentType)
	}

	req.Header.Set("Authorization", q.makeAuthHeader(url, time.Now()))
	return req, nil
}

func (q *QueueClient) getClient() HttpClient {

	if httpClientOverride != nil {
		return httpClientOverride
	}

	if q.httpClient != nil {
		return q.httpClient
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.httpClient == nil {
		q.httpClient = &http.Client{}
	}

	return q.httpClient
}

// Creates an authenticaiton header with Shared Access Signature token.
//
// For more information see: https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-sas
func (q *QueueClient) makeAuthHeader(uri string, from time.Time) string {

	const expireInSeconds = 300

	epoch := from.Add(expireInSeconds * time.Second).Round(time.Second).Unix()
	expiry := strconv.Itoa(int(epoch))

	// as per https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-sas
	encodedUri := strings.ToLower(url.QueryEscape(uri))
	sig := q.makeSignatureString(encodedUri + "\n" + expiry)
	return fmt.Sprintf("SharedAccessSignature sig=%s&se=%s&skn=%s&sr=%s", sig, expiry, q.KeyName, encodedUri)
}

// Returns SHA-256 hash of the scope of the token with a CRLF appended and an expiry time.
func (q *QueueClient) makeSignatureString(s string) string {
	// as per https://docs.microsoft.com/en-us/azure/service-bus-messaging/service-bus-sas
	h := hmac.New(sha256.New, []byte(q.KeyValue))
	h.Write([]byte(s))
	encodedSig := base64.StdEncoding.EncodeToString(h.Sum(nil))
	return url.QueryEscape(encodedSig)
}
type NoMessagesAvailableError struct {
	Code int
	Body string
}

func (e NoMessagesAvailableError) Error() string {
	return "No messages available within the specified timeout period"
}

type BadRequestError struct {
	Code int
	Body string
}

func (e BadRequestError) Error() string {
	return "Bad createRequest"
}

type NotAuthorizedError struct {
	Code int
	Body string
}

func (e NotAuthorizedError) Error() string {
	return "Authorization failure"
}

type MessageDontExistError struct {
	Code int
	Body string
}

func (e MessageDontExistError) Error() string {
	return "No message was found with the specified MessageId or LockToken."
}

type QueueDontExistError struct {
	Code int
	Body string
}

func (e QueueDontExistError) Error() string {
	return "Specified queue or subscription does not exist"
}

type InternalError struct {
	Code int
	Body string
}

func (e InternalError) Error() string {
	return "Internal Error"
}

func wrap(err error, message string) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf(message + ": " + err.Error())
}
func handleStatusCode(resp *http.Response) error {

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}

	body, _ := ioutil.ReadAll(resp.Body)

	switch resp.StatusCode {
	case 204:
		return NoMessagesAvailableError{204, string(body)}
	case 400:
		return BadRequestError{400, string(body)}
	case 401:
		return NotAuthorizedError{401, string(body)}
	case 404:
		return MessageDontExistError{404, string(body)}
	case 410:
		return QueueDontExistError{410, string(body)}
	case 500:
		return InternalError{500, string(body)}
	}

	return fmt.Errorf("Unknown status %v with body %v", resp.StatusCode, string(body))
}

func parseMessage(resp *http.Response) (*Message, error) {

	//logger.Debug("Response StatusCode ", resp.StatusCode)
	//logger.Debug("Response Status ", resp.Status)
	//logger.Debug("Response Header ", resp.Header)
	//logger.Debug("Response ContentLength ", resp.ContentLength)

	m := Message{
		Properties: Properties{},
	}

	parseHeaders(&m, resp)

	brokerProperties := resp.Header.Get(headerBrokerProperties)

	if len(brokerProperties) > 0 {
		parseBrokerProperties(&m, brokerProperties)
	}

	value, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return nil, wrap(err, "Error reading message body")
	}

	m.Body = value

	return &m, nil
}

func parseHeaders(m *Message, resp *http.Response) {
	for k, v := range resp.Header {

		switch k {
		case headerBrokerProperties:
			{
				continue
			}
		case headerContentType:
			{
				m.ContentType = v[0]
				continue
			}
		case headerDate:
			{
				if t, err := time.Parse(Rfc2616Time, v[0]); err == nil {
					m.EnqueuedTimeUtc = t
				}
				continue
			}
		default:
			{
				// azure returns customer headers quoted
				m.Properties.Set(k, strings.Trim(v[0], "\""))
			}
		}
	}
}

func parseBrokerProperties(m *Message, properties string) {

	//logger.Debug("Response BrokerProperties ", properties)

	p := brokerProperties{}
	if err := json.Unmarshal([]byte(properties), &p); err != nil {
		//logger.Error("BrokerProperties header parse failed", err)
		return
	}

	m.Id = p.MessageId
	m.SessionId = p.SessionId
	m.LockToken = p.LockToken
	m.Label = p.Label
	m.ReplyTo = p.ReplyTo
	m.To = p.To
	m.CorrelationId = p.CorrelationId
	m.ReplyToSessionId = p.ReplyToSessionId
	m.PartitionKey = p.PartitionKey
	m.CorrelationId = p.CorrelationId
	m.DeliveryCount = p.DeliveryCount
	m.SequenceNumber = p.SequenceNumber
	m.TimeToLive = p.TimeToLive

	const Rfc2616Time = "Mon, 02 Jan 2006 15:04:05 MST"

	if t, err := time.Parse(Rfc2616Time, p.LockedUntilUtc); err == nil {
		m.LockedUntilUtc = t
	}

	if t, err := time.Parse(Rfc2616Time, p.ScheduledEnqueueTimeUtc); err == nil {
		m.ScheduledEnqueueTimeUtc = t
	}
}

// See https://docs.microsoft.com/en-us/rest/api/servicebus/message-headers-and-properties
type brokerProperties struct {
	// Req, Res
	MessageId string `json:"MessageId,omitempty"`

	// Req, Res
	Label string `json:"Label,omitempty"`

	// Req, Res
	CorrelationId string `json:"CorrelationId,omitempty"`

	// Req, Res
	SessionId string `json:"SessionId,omitempty"`

	// Req, Res
	TimeToLive int `json:"TimeToLive,omitempty"`

	// Req, Res
	To string `json:"To,omitempty"`

	// Req, Res
	ReplyTo string `json:"ReplyTo,omitempty"`

	// Req, Res
	ScheduledEnqueueTimeUtc string `json:"ScheduledEnqueueTimeUtc,omitempty"`

	// Req, Res
	ReplyToSessionId string `json:"ReplyToSessionId,omitempty"`

	// Req, Res
	PartitionKey string `json:"PartitionKey,omitempty"`

	// Res
	DeliveryCount int `json:"DeliveryCount,omitempty"`

	// Res
	LockToken string `json:"LockToken,omitempty"`

	// Res
	LockedUntilUtc string `json:"LockedUntilUtc,omitempty"`

	// Res
	SequenceNumber int64 `json:"SequenceNumber,omitempty"`
}

func (p *brokerProperties) CopyFromMessage(msg *Message) {
	p.MessageId = msg.Id
	p.Label = msg.Label
	p.CorrelationId = msg.CorrelationId
	p.SessionId = msg.SessionId
	p.TimeToLive = msg.TimeToLive
	p.To = msg.To
	p.ReplyTo = msg.ReplyTo
	p.ReplyToSessionId = msg.ReplyToSessionId
	p.PartitionKey = msg.PartitionKey

	defaultTime := time.Time{}
	if msg.ScheduledEnqueueTimeUtc != defaultTime {
		p.ScheduledEnqueueTimeUtc = msg.ScheduledEnqueueTimeUtc.Format(Rfc2616Time)
	}
}

func (p *brokerProperties) Marshal() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}

	return string(b), nil
}


// THIS HAS CHANGED
// Eval implements activity.Activity.Eval
func (a *MyActivity) Eval(context activity.Context) (done bool, err error)  {
	// Get the activity data from the context
	//name := context.GetInput("name").(string)
	vNamespace := context.GetInput("Namespace").(string)
	vKeyName := context.GetInput("KeyName").(string)
	vMessage := context.GetInput("Message").(string)
	vKeyValue := context.GetInput("KeyValue").(string)
	vQueueName := context.GetInput("QueueName").(string)
	vTimeout := context.GetInput("Timeout").(int)
	
	//log.Printf("Starting")

cli := QueueClient{
  Namespace:  vNamespace,
  KeyName:    vKeyName,
  KeyValue:   vKeyValue,
  QueueName:  vQueueName,
  Timeout:    vTimeout,
}


msg := NewMessage([]byte(vMessage))

msg.Properties.Set("Property1", "One")
msg.Properties.Set("Property2", "two")

// send message
cli.SendMessage(msg)

if err!= nil {
mylog.Printf(err.Error())
context.SetOutput("Code", string("Error"))
context.SetOutput("result", string(err.Error()))
}

	// Use the log object to log the greeting
	log.Debugf("The Flogo engine says  [%s] to [%s]", vNamespace, vKeyName)

	// Set the result as part of the context
	context.SetOutput("Code", string("Success"))
	context.SetOutput("result", "The Flogo engine says  "+vNamespace+" to "+vKeyName)

	// Signal to the Flogo engine that the activity is completed
	return true, nil
}