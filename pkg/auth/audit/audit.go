package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/endpoints/request"
)

const (
	contentTypeJSON = "application/json"
	redacted        = "[redacted]"
)

const (
	levelNull = iota
	levelMetadata
	levelRequest
	levelRequestResponse
)

var (
	bodyMethods = map[string]bool{
		http.MethodPut:  true,
		http.MethodPost: true,
	}
	sensitiveRequestHeader  = []string{"Cookie", "Authorization"}
	sensitiveResponseHeader = []string{"Cookie", "Set-Cookie"}
)

type auditLog struct {
	log                *log
	writer             *LogWriter
	reqBody            []byte
	keysToConcealRegex *regexp.Regexp
}

type log struct {
	AuditID           k8stypes.UID `json:"auditID,omitempty"`
	RequestURI        string       `json:"requestURI,omitempty"`
	User              *User        `json:"user,omitempty"`
	Method            string       `json:"method,omitempty"`
	RemoteAddr        string       `json:"remoteAddr,omitempty"`
	RequestTimestamp  string       `json:"requestTimestamp,omitempty"`
	ResponseTimestamp string       `json:"responseTimestamp,omitempty"`
	ResponseCode      int          `json:"responseCode,omitempty"`
	RequestHeader     http.Header  `json:"requestHeader,omitempty"`
	ResponseHeader    http.Header  `json:"responseHeader,omitempty"`
	RequestBody       []byte       `json:"requestBody,omitempty"`
	ResponseBody      []byte       `json:"responseBody,omitempty"`
}

var userKey struct{}

type User struct {
	Name  string   `json:"name,omitempty"`
	Group []string `json:"group,omitempty"`
	// RequestUser is the --as user
	RequestUser string `json:"requestUser,omitempty"`
	// RequestGroups is the --as-group list
	RequestGroups []string `json:"requestGroups,omitempty"`
}

func getUserInfo(req *http.Request) *User {
	user, _ := request.UserFrom(req.Context())
	return &User{
		Name:  user.GetName(),
		Group: user.GetGroups(),
	}
}

func FromContext(ctx context.Context) (*User, bool) {
	u, ok := ctx.Value(userKey).(*User)
	return u, ok
}

func newAuditLog(writer *LogWriter, req *http.Request, keysToConcealRegex *regexp.Regexp) (*auditLog, error) {
	auditLog := &auditLog{
		writer: writer,
		log: &log{
			AuditID:          k8stypes.UID(uuid.NewRandom().String()),
			RequestURI:       req.RequestURI,
			Method:           req.Method,
			RemoteAddr:       req.RemoteAddr,
			RequestTimestamp: time.Now().Format(time.RFC3339),
		},
		keysToConcealRegex: keysToConcealRegex,
	}

	contentType := req.Header.Get("Content-Type")
	if writer.Level >= levelRequest && bodyMethods[req.Method] && contentType == contentTypeJSON {
		reqBody, err := readBodyWithoutLosingContent(req)
		if err != nil {
			return nil, err
		}
		auditLog.reqBody = reqBody
	}
	return auditLog, nil
}

func (a *auditLog) write(userInfo *User, reqHeaders, resHeaders http.Header, resCode int, resBody []byte) error {
	a.log.User = userInfo
	a.log.ResponseTimestamp = time.Now().Format(time.RFC3339)
	a.log.RequestHeader = filterOutHeaders(reqHeaders, sensitiveRequestHeader)
	a.log.ResponseHeader = filterOutHeaders(resHeaders, sensitiveResponseHeader)
	a.log.ResponseCode = resCode

	var buffer bytes.Buffer
	alByte, err := json.Marshal(a.log)
	if err != nil {
		return err
	}

	buffer.Write(bytes.TrimSuffix(alByte, []byte("}")))
	if a.writer.Level >= levelRequest && len(a.reqBody) > 0 {
		buffer.WriteString(`,"requestBody":`)
		buffer.Write(bytes.TrimSuffix(a.concealSensitiveData(a.log.RequestURI, a.reqBody), []byte("\n")))
	}
	if a.writer.Level >= levelRequestResponse && resHeaders.Get("Content-Type") == contentTypeJSON && len(resBody) > 0 {
		buffer.WriteString(`,"responseBody":`)
		buffer.Write(bytes.TrimSuffix(a.concealSensitiveData(a.log.RequestURI, resBody), []byte("\n")))
	}
	buffer.WriteString("}")

	var compactBuffer bytes.Buffer
	err = json.Compact(&compactBuffer, buffer.Bytes())
	if err != nil {
		return errors.Wrap(err, "compact audit log json failed")
	}

	compactBuffer.WriteString("\n")
	_, err = a.writer.Output.Write(compactBuffer.Bytes())
	return err
}

func readBodyWithoutLosingContent(req *http.Request) ([]byte, error) {
	if !bodyMethods[req.Method] {
		return nil, nil
	}

	bodyBytes, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))

	return bodyBytes, nil
}

func filterOutHeaders(headers http.Header, filterKeys []string) map[string][]string {
	newHeader := make(map[string][]string)
	for k, v := range headers {
		if isExist(filterKeys, k) {
			continue
		}
		newHeader[k] = v
	}
	return newHeader
}

func isExist(array []string, key string) bool {
	for _, v := range array {
		if v == key {
			return true
		}
	}
	return false
}

func (a *auditLog) concealSensitiveData(requestURI string, body []byte) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}

	var changed bool
	// Conceal values of secret data.
	if strings.Contains(requestURI, "secrets") {
		dataKey := "data"
		data, _ := m[dataKey].(map[string]interface{})
		if data == nil {
			dataKey = "stringData"
			data, _ = m[dataKey].(map[string]interface{})
		}

		for key := range data {
			data[key] = redacted
		}
		if data != nil {
			changed = true
			m[dataKey] = data
		}
	}

	// Conceal values for data considered sensitive: passwords, tokens, etc.
	if !a.concealMap(m) && !changed {
		return body
	}

	newBody, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return newBody
}

func (a *auditLog) concealMap(m map[string]interface{}) bool {
	var changed bool
	for key := range m {
		if _, ok := m[key].(string); ok {
			if a.keysToConcealRegex.MatchString(key) {
				changed = true
				m[key] = redacted
			}
		} else if nested, ok := m[key].(map[string]interface{}); ok && a.concealMap(nested) {
			changed = true
			m[key] = nested
		}
	}

	return changed
}
