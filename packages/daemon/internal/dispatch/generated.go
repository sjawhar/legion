package dispatch

// MessageBodyLimit is the longest body Dispatch accepts on a message, in UTF-16 code units as
// Dispatch counts them. Generated from DISPATCH_BODY_MAX in packages/contracts;
// record.MessagePostLimit subtracts the outbox marker the runner appends from it.
const MessageBodyLimit = 32000
