package httptransport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const maximumJSONBodyBytes = 16 << 10

var (
	errInvalidJSON          = errors.New("invalid JSON request")
	errBodyTooLarge         = errors.New("request body too large")
	errUnsupportedMediaType = errors.New("content type must be application/json")
)

func decodeJSON(c *gin.Context, destination any) error {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errUnsupportedMediaType
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maximumJSONBodyBytes)
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return errBodyTooLarge
		}
		return errInvalidJSON
	}
	if err := validateJSONKeys(data); err != nil {
		return errInvalidJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return errBodyTooLarge
		}
		return errInvalidJSON
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return errBodyTooLarge
		}
		return errInvalidJSON
	}
	return nil
}

func validateJSONKeys(data []byte) error {
	if !utf8.Valid(data) {
		return errInvalidJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, true); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errInvalidJSON
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, requireObject bool) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		if requireObject {
			return errInvalidJSON
		}
		if value, ok := token.(string); ok && strings.ContainsRune(value, utf8.RuneError) {
			return errInvalidJSON
		}
		return nil
	}
	if requireObject && delimiter != json.Delim('{') {
		return errInvalidJSON
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errInvalidJSON
			}
			if key != strings.ToLower(key) {
				return errInvalidJSON
			}
			if _, exists := keys[key]; exists {
				return errInvalidJSON
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder, false); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errInvalidJSON
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder, false); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errInvalidJSON
		}
	default:
		return errInvalidJSON
	}
	return nil
}

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

type apiErrorEnvelope struct {
	Error apiError `json:"error"`
}

func writeAPIError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, apiErrorEnvelope{Error: apiError{
		Code:      code,
		Message:   message,
		RequestID: requestIDFromContext(c),
	}})
}

func writeDecodeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errBodyTooLarge):
		writeAPIError(c, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body is too large")
	case errors.Is(err, errUnsupportedMediaType):
		writeAPIError(c, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "content type must be application/json")
	default:
		writeAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
	}
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n,") {
		return "", false
	}
	return token, true
}
