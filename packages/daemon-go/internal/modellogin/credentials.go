package modellogin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// LoginDocument is the complete Secrets Manager document agent-c supplies for the daemon's
// machine user. Cognito InitiateAuth submits the client id and user credentials; the user-pool id
// still travels with the document so the configured command cannot silently select another pool.
type LoginDocument struct {
	UserPoolID string `json:"user_pool_id"`
	Region     string `json:"region"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	ClientID   string `json:"client_id"`
}

// ParseLoginDocument reads the machine login document the operator's command printed. Its error
// deliberately never includes document text because it can contain the password the daemon holds.
func ParseLoginDocument(text string) (LoginDocument, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(text))
	decoder.DisallowUnknownFields()
	var document LoginDocument
	if err := decoder.Decode(&document); err != nil || document.UserPoolID == "" || document.Region == "" ||
		document.Username == "" || document.Password == "" || document.ClientID == "" {
		return LoginDocument{}, errors.New("model login document must be a JSON object with non-empty user_pool_id, region, username, password, and client_id")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return LoginDocument{}, errors.New("model login document must contain one JSON object")
	}
	return document, nil
}
