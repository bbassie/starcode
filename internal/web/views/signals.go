package views

import "strings"

// Signals whose name starts with an underscore stay out of requests by
// default; that is where the composer's attachments live, so a request
// does not carry them unless it asks. withSignal is the fetch option that
// adds one of them to the normal set.
func withSignal(name string) string {
	return "{filterSignals: {exclude: /^_(?!" + strings.TrimPrefix(name, "_") + "$)/}}"
}

// urlQuery escapes s for a query value inside a Datastar expression, where
// a quote would end the string.
func urlQuery(s string) string {
	r := strings.NewReplacer("%", "%25", "&", "%26", "#", "%23", "+", "%2B", " ", "%20", "'", "%27", "?", "%3F")
	return r.Replace(s)
}
