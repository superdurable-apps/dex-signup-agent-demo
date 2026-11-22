package signuptest

// A Gmail API stand-in for the tests that run send_welcome_email on the real Gmail connector: users.messages.send and
// the Sent Mail read-back that the connector's fence makes. GmailMailbox models Gmail as the connector's read-back
// meets it (superdurable/dex-connectors-library#1061): messages.send replaces the client's Message-ID on the Sent copy
// with Gmail's own and keeps the X-Dex-Message-ID header, so a search by rfc822msgid: never finds the client's value;
// a Sent Mail search lists recent candidates, newest first; and a format=metadata read returns a candidate's headers.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// GmailSend is one message Gmail accepted.
type GmailSend struct {
	// MessageID is the Message-ID the client sent; Gmail keeps no copy of it.
	MessageID string
	// DexMessageID is the X-Dex-Message-ID header the client sent, kept on the Sent copy.
	DexMessageID string
	// SentMessageID is the Message-ID of the Sent copy, Gmail's own.
	SentMessageID string
	To            string
	Subject       string
	Raw           string
	// GmailID is the ID Gmail gave the message.
	GmailID string
}

// GmailMailbox is one mailbox's Sent Mail, with the requests that read it.
type GmailMailbox struct {
	mutex       sync.Mutex
	sends       []GmailSend
	searches    []string
	headerReads []string
}

var (
	messageIDHeader    = regexp.MustCompile(`(?mi)^Message-ID: *(.*?)\r?$`)
	dexMessageIDHeader = regexp.MustCompile(`(?mi)^X-Dex-Message-ID: *(.*?)\r?$`)
	toHeader           = regexp.MustCompile(`(?mi)^To: *(.*?)\r?$`)
	subjectHeader      = regexp.MustCompile(`(?mi)^Subject: *(.*?)\r?$`)
	searchMessageID    = regexp.MustCompile(`rfc822msgid:(\S+)`)
	searchRecipient    = regexp.MustCompile(`(?:^| )to:(\S+)`)
)

func headerOf(pattern *regexp.Regexp, raw string) string {
	if match := pattern.FindStringSubmatch(raw); match != nil {
		return match[1]
	}
	return ""
}

// DecodeSendBody is the RFC 5322 message of a messages.send request body: its raw field, base64url with or without
// padding.
func DecodeSendBody(body []byte) (string, error) {
	var request struct {
		Raw string `json:"raw"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return "", err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(request.Raw, "="))
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// Send records raw in Sent Mail as Gmail accepts it: under a new ID, with Gmail's own Message-ID.
func (mailbox *GmailMailbox) Send(raw string) GmailSend {
	mailbox.mutex.Lock()
	defer mailbox.mutex.Unlock()
	number := len(mailbox.sends) + 1
	send := GmailSend{
		MessageID: headerOf(messageIDHeader, raw), DexMessageID: headerOf(dexMessageIDHeader, raw),
		SentMessageID: fmt.Sprintf("<CAgmail%d@mail.gmail.com>", number),
		To:            headerOf(toHeader, raw), Subject: headerOf(subjectHeader, raw), Raw: raw,
		GmailID: fmt.Sprintf("gmail-%d", number),
	}
	mailbox.sends = append(mailbox.sends, send)
	return send
}

// SendAnswer is Gmail's answer to an accepted send.
func SendAnswer(send GmailSend) map[string]any {
	return map[string]any{"id": send.GmailID, "threadId": "thread-" + send.GmailID, "labelIds": []string{"SENT"}}
}

// Sends are the messages in Sent Mail, oldest first.
func (mailbox *GmailMailbox) Sends() []GmailSend {
	mailbox.mutex.Lock()
	defer mailbox.mutex.Unlock()
	return append([]GmailSend(nil), mailbox.sends...)
}

// Searches are the q values of the message searches, in order.
func (mailbox *GmailMailbox) Searches() []string {
	mailbox.mutex.Lock()
	defer mailbox.mutex.Unlock()
	return append([]string(nil), mailbox.searches...)
}

// HeaderReads are the IDs of the messages read, in order.
func (mailbox *GmailMailbox) HeaderReads() []string {
	mailbox.mutex.Lock()
	defer mailbox.mutex.Unlock()
	return append([]string(nil), mailbox.headerReads...)
}

// Answer answers a request to the Gmail API other than messages.send: a message search or a message read. Any other
// request is 404.
func (mailbox *GmailMailbox) Answer(method string, target *url.URL) (int, any) {
	path := target.Path
	if method != http.MethodGet || !strings.Contains(path, "/users/me/messages") {
		return http.StatusNotFound, notFound()
	}
	if strings.HasSuffix(path, "/users/me/messages") {
		return http.StatusOK, mailbox.search(target.Query())
	}
	id, err := url.PathUnescape(path[strings.LastIndex(path, "/")+1:])
	if err != nil {
		return http.StatusNotFound, notFound()
	}
	return mailbox.read(id, target.Query())
}

func notFound() map[string]any {
	return map[string]any{"error": map[string]any{"code": 404, "message": "Requested entity was not found."}}
}

// search lists the Sent copies a query finds, newest first. rfc822msgid: matches a Sent copy's own Message-ID; in:sent
// lists Sent Mail, narrowed by to:. Gmail leaves messages out when none match.
func (mailbox *GmailMailbox) search(query url.Values) map[string]any {
	q := query.Get("q")
	limit := 100
	if value, err := strconv.Atoi(query.Get("maxResults")); err == nil && value > 0 {
		limit = value
	}
	mailbox.mutex.Lock()
	defer mailbox.mutex.Unlock()
	mailbox.searches = append(mailbox.searches, q)
	var messages []any
	for index := len(mailbox.sends) - 1; index >= 0 && len(messages) < limit; index-- {
		send := mailbox.sends[index]
		switch match := searchMessageID.FindStringSubmatch(q); {
		case match != nil:
			if strings.Trim(match[1], "<>") != strings.Trim(send.SentMessageID, "<>") {
				continue
			}
		case strings.Contains(q, "in:sent"):
			if recipient := searchRecipient.FindStringSubmatch(q); recipient != nil &&
				!strings.Contains(strings.ToLower(send.To), strings.ToLower(recipient[1])) {
				continue
			}
		default:
			continue
		}
		messages = append(messages, map[string]any{"id": send.GmailID, "threadId": "thread-" + send.GmailID})
	}
	if len(messages) == 0 {
		return map[string]any{"resultSizeEstimate": 0}
	}
	return map[string]any{"messages": messages, "resultSizeEstimate": len(messages)}
}

// read is a message resource with its headers: those metadataHeaders names, for format=metadata.
func (mailbox *GmailMailbox) read(id string, query url.Values) (int, any) {
	mailbox.mutex.Lock()
	defer mailbox.mutex.Unlock()
	mailbox.headerReads = append(mailbox.headerReads, id)
	for _, send := range mailbox.sends {
		if send.GmailID != id {
			continue
		}
		headers := []map[string]string{
			{"name": "Message-ID", "value": send.SentMessageID},
			{"name": "X-Dex-Message-ID", "value": send.DexMessageID},
			{"name": "To", "value": send.To},
			{"name": "Subject", "value": send.Subject},
		}
		if wanted := query["metadataHeaders"]; query.Get("format") == "metadata" && len(wanted) > 0 {
			var kept []map[string]string
			for _, header := range headers {
				for _, name := range wanted {
					if strings.EqualFold(header["name"], name) {
						kept = append(kept, header)
						break
					}
				}
			}
			headers = kept
		}
		return http.StatusOK, map[string]any{
			"id": send.GmailID, "threadId": "thread-" + send.GmailID, "labelIds": []string{"SENT"},
			"payload": map[string]any{"mimeType": "text/plain", "headers": headers},
		}
	}
	return http.StatusNotFound, notFound()
}

// GmailAPI is the stand-in, an HTTP server over a GmailMailbox. HoldNextSend makes the next send record the message as
// sent and then wait without an answer, as a provider whose answer is lost: the caller's connection then ends.
type GmailAPI struct {
	*GmailMailbox
	server   *httptest.Server
	mutex    sync.Mutex
	holdNext bool
	held     chan struct{}
	release  chan struct{}
}

// StartGmailAPI starts the stand-in.
func StartGmailAPI() *GmailAPI {
	api := &GmailAPI{GmailMailbox: &GmailMailbox{}, held: make(chan struct{}, 8), release: make(chan struct{})}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	return api
}

// Endpoint is the Gmail API endpoint of the stand-in, the welcome-gmail connection's endpoint setting.
func (api *GmailAPI) Endpoint() string { return api.server.URL + "/gmail/v1" }

// Stop releases a held send and stops the stand-in.
func (api *GmailAPI) Stop() {
	api.mutex.Lock()
	select {
	case <-api.release:
	default:
		close(api.release)
	}
	api.mutex.Unlock()
	api.server.CloseClientConnections()
	api.server.Close()
}

// HoldNextSend holds the answer to the next send; Held signals when a send is held.
func (api *GmailAPI) HoldNextSend() {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.holdNext = true
}

// Held is signalled each time a send is held.
func (api *GmailAPI) Held() <-chan struct{} { return api.held }

func (api *GmailAPI) serve(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/users/me/messages/send") {
		status, body := api.Answer(request.Method, request.URL)
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(status)
		_ = json.NewEncoder(response).Encode(body)
		return
	}
	body, _ := io.ReadAll(request.Body)
	raw, err := DecodeSendBody(body)
	if err != nil {
		response.WriteHeader(http.StatusBadRequest)
		writeJSON(response, map[string]any{"error": map[string]any{"code": 400, "message": "Invalid value for ByteString"}})
		return
	}
	send := api.Send(raw)
	api.mutex.Lock()
	hold := api.holdNext
	api.holdNext = false
	api.mutex.Unlock()
	if hold {
		api.held <- struct{}{}
		select {
		case <-api.release:
		case <-request.Context().Done():
		}
		return
	}
	writeJSON(response, SendAnswer(send))
}
