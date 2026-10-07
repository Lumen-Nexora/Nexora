package nexora

import (
	"encoding/json"
	"fmt"
)

type NexoraError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status"`
	Details    any    `json:"details,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

func (e *NexoraError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("nexora: %s (%d): %s", e.Code, e.HTTPStatus, e.Message)
}

func parseError(status int, body []byte, retryAfter int) *NexoraError {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details any    `json:"details"`
		} `json:"error"`
		ValidationErrors any `json:"validation_errors"`
	}
	_ = json.Unmarshal(body, &envelope)
	code, message, details := envelope.Error.Code, envelope.Error.Message, envelope.Error.Details
	if envelope.ValidationErrors != nil {
		details = envelope.ValidationErrors
	}
	if code == "" {
		code = "UNKNOWN_ERROR"
	}
	if message == "" {
		message = fmt.Sprintf("request failed with status %d", status)
	}
	return &NexoraError{Code: code, Message: message, HTTPStatus: status, Details: details, RetryAfter: retryAfter}
}
