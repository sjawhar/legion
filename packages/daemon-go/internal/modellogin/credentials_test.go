package modellogin

import (
	"strings"
	"testing"
)

func TestParseLoginDocumentReadsEveryMachineLoginField(t *testing.T) {
	document, err := ParseLoginDocument(`{"user_pool_id":"pool-id","region":"example-region-1","username":"machine-user","password":"machine-password","client_id":"client-id"}`)
	if err != nil {
		t.Fatalf("ParseLoginDocument: %v", err)
	}
	if document != (LoginDocument{
		UserPoolID: "pool-id", Region: "example-region-1", Username: "machine-user",
		Password: "machine-password", ClientID: "client-id",
	}) {
		t.Fatalf("document = %#v", document)
	}
}

func TestParseLoginDocumentRefusesAnInvalidDocumentWithoutEchoingIt(t *testing.T) {
	const secret = "machine-password"
	_, err := ParseLoginDocument(`{"user_pool_id":"pool-id","region":"example-region-1","username":"machine-user","password":"` + secret + `","client_id":"client-id","unexpected":true}`)
	if err == nil {
		t.Fatal("ParseLoginDocument accepted an unexpected login document field")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("ParseLoginDocument error leaks the document password: %v", err)
	}
}
