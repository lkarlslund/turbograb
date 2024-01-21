package turbograb

import "crypto/x509"

//go:generate easyjson types.go

//easyjson:json
type Result struct {
	Site         string              `json:"site,omitempty" bson:"site,omitempty"`
	URL          string              `json:"url,omitempty" bson:"url,omitempty"`
	IPaddress    string              `json:"ip,omitempty" bson:"ip,omitempty"`
	Code         int                 `json:"resultcode,omitempty" bson:"resultcode,omitempty"`
	Certificates []*x509.Certificate `json:"certificates,omitempty" bson:"certificates,omitempty"`
	Error        string              `json:"error,omitempty" bson:"error,omitempty"`
	Warnings     []string            `json:"warnings,omitempty" bson:"warnings,omitempty"`
	Body         string              `json:"body,omitempty" bson:"body,omitempty"`
	Header       string              `json:"headers,omitempty" bson:"headers,omitempty"`
}

type Encoded struct {
	Site string
	Data []byte
}
