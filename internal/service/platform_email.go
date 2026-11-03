package service

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
)

// ---- Platform emails (SaaS) ----------------------------------------------------
// Sign-in links and org invites come from the platform, not from one event,
// so they carry the product name instead of legacy artwork.

// ActionEmail is a short message with one button.
type ActionEmail struct {
	Product   string   // platform name for the header
	Heading   string   // one line under the header
	Paragraph []string // body text, one paragraph each
	Button    string
	URL       string
	Footnote  string
}

var actionHTML = template.Must(template.New("action").Parse(`<!DOCTYPE html>
<html lang="en"><body style="margin:0;padding:0;background:#EEF2F0;font-family:Arial,Helvetica,sans-serif;color:#15202B;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#EEF2F0;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:520px;background:#FFFFFF;border-radius:14px;overflow:hidden;">
  <tr><td style="background:#15202B;padding:20px 26px;color:#FFFFFF;">
    <div style="font-size:20px;font-weight:bold;">{{.Product}}</div>
    <div style="font-size:14px;opacity:.85;margin-top:4px;">{{.Heading}}</div>
  </td></tr>
  <tr><td style="padding:24px 26px 8px;font-size:15px;line-height:1.55;">
    {{range .Paragraph}}<p style="margin:0 0 14px;">{{.}}</p>{{end}}
  </td></tr>
  <tr><td style="padding:6px 26px 6px;" align="center">
    <a href="{{.URL}}" style="display:inline-block;background:#2350C8;color:#FFFFFF;text-decoration:none;font-weight:bold;padding:13px 26px;border-radius:8px;font-size:15px;">{{.Button}}</a>
    <div style="font-size:12px;color:#5B6874;margin-top:8px;word-break:break-all;">{{.URL}}</div>
  </td></tr>
  <tr><td style="padding:16px 26px 24px;font-size:13px;line-height:1.55;color:#5B6874;">
    <p style="margin:0;">{{.Footnote}}</p>
  </td></tr>
</table>
</td></tr></table>
</body></html>`))

// Render returns the plain-text and HTML bodies.
func (e ActionEmail) Render() (text, html string, err error) {
	var t strings.Builder
	for _, p := range e.Paragraph {
		t.WriteString(p + "\n\n")
	}
	fmt.Fprintf(&t, "%s: %s\n\n%s\n", e.Button, e.URL, e.Footnote)

	var h bytes.Buffer
	if err := actionHTML.Execute(&h, e); err != nil {
		return "", "", fmt.Errorf("render email: %w", err)
	}
	return t.String(), h.String(), nil
}
