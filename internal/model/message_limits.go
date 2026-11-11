package model

// MaxMessageBodyBytes is the fixed raw UTF-8 body ceiling. A configuration may
// lower fresh-message admission, but increasing this ceiling needs storage and
// transport validation, including escaped JSON and durable reply intents.
const MaxMessageBodyBytes = 16 * 1024
