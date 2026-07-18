package exec //nolint:revive // internal package, always imported with alias

import (
	"encoding/json"

	"github.com/gastownhall/gascity/internal/mail"
)

// sendInput is the JSON wire format sent to the script's stdin on Send.
type sendInput struct {
	From    string `json:"from"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// replyInput is the JSON wire format sent to the script's stdin on Reply.
type replyInput struct {
	From    string `json:"from"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// countOutput is the JSON wire format returned by the script on Count.
type countOutput struct {
	Total  int `json:"total"`
	Unread int `json:"unread"`
}

// marshalSendInput encodes the send payload as JSON.
func marshalSendInput(from, subject, body string) ([]byte, error) {
	return json.Marshal(sendInput{From: from, Subject: subject, Body: body})
}

// marshalReplyInput encodes the reply payload as JSON.
func marshalReplyInput(from, subject, body string) ([]byte, error) {
	return json.Marshal(replyInput{From: from, Subject: subject, Body: body})
}

// unmarshalMessage decodes a single Message from JSON.
func unmarshalMessage(data string) (mail.Message, error) {
	var m mail.Message
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return mail.Message{}, err
	}
	return m, nil
}

// unmarshalMessages decodes a JSON array of Messages.
func unmarshalMessages(data string) ([]mail.Message, error) {
	var msgs []mail.Message
	if err := json.Unmarshal([]byte(data), &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// unmarshalCount decodes the count output JSON.
func unmarshalCount(data string) (int, int, error) {
	var c countOutput
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return 0, 0, err
	}
	return c.Total, c.Unread, nil
}
