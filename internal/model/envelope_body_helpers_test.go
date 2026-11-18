package model

// envelopeBody escapes display controls and marker delimiters without changing storage.
func envelopeBody(body string) string { return envelopeBodyBudget(body, envelopeJSONBytes) }
