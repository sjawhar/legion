package testnats

import "strings"

// LegacyKeys returns two keys, each of letter, that a listener built before the KV key check could
// have stored in the bucket named bucket, and that this one refuses to write. That listener read
// every key with a direct get before it wrote one, and the server takes that request's protocol
// line, `PUB $JS.API.DIRECT.GET.KV_<bucket>.$KV.<bucket>.<key> <38-byte reply inbox> 0`, up to its
// 4 KiB max_control_line, so its keys run to 4,027 bytes less twice the bucket name. readable is
// past this listener's write bound but within its read bound, so a store still reads it;
// unreadable is past the read bound too, so a store can only list and delete it.
func LegacyKeys(bucket string, letter string) (readable, unreadable string) {
	return strings.Repeat(letter, 3997-2*len(bucket)), strings.Repeat(letter, 4015-2*len(bucket))
}

// RawOnlyKey returns a key, of letter, past the bound a listener deletes a key of the bucket named
// bucket to, which no listener could have stored but a plain KV put from another writer still can:
// its put, `PUB $KV.<bucket>.<key> <38-byte reply inbox> <size>`, fits the server's 4 KiB line.
func RawOnlyKey(bucket string, letter string) string {
	return strings.Repeat(letter, 4035-len(bucket))
}
