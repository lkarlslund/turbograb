package turbograb

import "crypto/x509"

//go:generate easyjson types.go

//easyjson:json
type Result struct {
	Site         string              `json:"site,omitempty"`
	URL          string              `json:"url,omitempty"`
	Code         int                 `json:"resultcode,omitempty"`
	Certificates []*x509.Certificate `json:"certificates,omitempty"`
	Error        string              `json:"error,omitempty"`
	Warnings     []string            `json:"warnings,omitempty"`
	Body         string              `json:"body,omitempty"`
	Header       string              `json:"headers,omitempty"`
}

type Encoded struct {
	Site string
	Data []byte
}
