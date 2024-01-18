package main

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

type logger struct {
}

func (l logger) Errorf(format string, v ...interface{}) {}
func (l logger) Warnf(format string, v ...interface{})  {}
func (l logger) Debugf(format string, v ...interface{}) {}

type encoded struct {
	Site string
	Data []byte
}
