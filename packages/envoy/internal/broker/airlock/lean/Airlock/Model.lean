/-!
# Airlock core model

The broker swaps placeholder `P` of grant `G` (for secret `S`) with `S`'s real value,
only in the request headers the rules name for `S`, only on a CONNECT host the rules
bind to `S`, and only when the TLS name and the Host header equal that host. The query
and the body are never touched.

Strings are Lean `String`s (valid UTF-8); the Go side refuses to send anything else.
Basic credentials are decoded in the model itself (base64, Go `StdEncoding` semantics:
CR/LF ignored, padding required, non-zero trailing bits accepted); the decoded
`user:pass` must be valid UTF-8 to count as a Basic credential.

This file is the model of `packages/envoy/internal/broker/airlock/airlock.go`: every
definition here has a Go function of the same name, and the differential fuzz test drives
the two against each other. A change on one side is a change on the other.
-/
namespace Airlock

/-! ## Data -/

structure Allow where
  method : String
  pathPrefix : String
  deriving Repr, BEq

structure SecretRule where
  name : String
  hosts : List String
  /-- `none` means the default `["authorization"]`. Compared ASCII-case-insensitively. -/
  headers : Option (List String) := none
  allow : Option (List Allow) := none
  /-- `""`, `"aws"` or `"azure"`. A plain `String`, not an `Option`, because the Go side
  sends the empty string for an entry with no kind. -/
  kind : String := ""
  /-- Judge lifetime in seconds. -/
  judge : Option Nat := none
  deriving Repr

structure Rules where
  secrets : List SecretRule
  deriving Repr

structure Grant where
  secret : String
  placeholder : String
  deriving Repr

structure Request where
  connectHost : String
  /-- The port the CONNECT named. Only 443 may carry a value: the three names the decision
  compares carry no port, so 443 is the only port the rules speak about, and another port of
  one name is another service. -/
  connectPort : Nat
  sni : String
  hostHeader : String
  method : String
  path : String
  headers : List (String × String)
  query : String
  body : String
  deriving Repr

inductive Decision where
  | allow
  | refuse (reason : String)
  deriving Repr, BEq

/-! ## String helpers (on `List Char`, so they are total and plain to reason about) -/

/-- ASCII-only lower-casing (`Char.toLower` touches only `A`–`Z`). -/
def lowerAscii (s : String) : String := s.map Char.toLower

/-- `pat` occurs in `s` as a contiguous run. -/
def containsL : List Char → List Char → Bool
  | [], pat => pat.isEmpty
  | c :: cs, pat => pat.isPrefixOf (c :: cs) || containsL cs pat

def contains (s pat : String) : Bool := containsL s.toList pat.toList

/-- Replace each leftmost, non-overlapping occurrence of a non-empty `pat` by `rep`.
`fuel` ≥ length of the input suffices: every step consumes at least one char. -/
def replaceL (pat rep : List Char) : Nat → List Char → List Char
  | 0, s => s
  | _ + 1, [] => []
  | n + 1, c :: cs =>
    if !pat.isEmpty && pat.isPrefixOf (c :: cs) then
      rep ++ replaceL pat rep n ((c :: cs).drop pat.length)
    else c :: replaceL pat rep n cs

def replaceAll (s pat rep : String) : String :=
  String.ofList (replaceL pat.toList rep.toList s.length s.toList)

/-- Trim HTTP optional whitespace (SP, HTAB) at both ends. -/
def trimOWS (cs : List Char) : List Char :=
  let ows : Char → Bool := fun c => c == ' ' || c == '\t'
  ((cs.dropWhile ows).reverse.dropWhile ows).reverse

/-! ## Base64 (standard alphabet, padded) -/

def b64Val (c : Char) : Option Nat :=
  if 'A' ≤ c ∧ c ≤ 'Z' then some (c.toNat - 'A'.toNat)
  else if 'a' ≤ c ∧ c ≤ 'z' then some (c.toNat - 'a'.toNat + 26)
  else if '0' ≤ c ∧ c ≤ '9' then some (c.toNat - '0'.toNat + 52)
  else if c = '+' then some 62
  else if c = '/' then some 63
  else none

def b64Alphabet : String := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

def b64Char (n : Nat) : Char := (b64Alphabet.toList.getD (n % 64) 'A')

def byte (n : Nat) : UInt8 := (n % 256).toUInt8

def decodeQuads : List Char → Option (List UInt8)
  | [] => some []
  | [a, b, '=', '='] => do
    let x ← b64Val a; let y ← b64Val b
    pure [byte (x * 4 + y / 16)]
  | [a, b, c, '='] => do
    let x ← b64Val a; let y ← b64Val b; let z ← b64Val c
    pure [byte (x * 4 + y / 16), byte ((y % 16) * 16 + z / 4)]
  | a :: b :: c :: d :: rest => do
    let x ← b64Val a; let y ← b64Val b; let z ← b64Val c; let w ← b64Val d
    let tail ← decodeQuads rest
    pure (byte (x * 4 + y / 16) :: byte ((y % 16) * 16 + z / 4) :: byte ((z % 4) * 64 + w) :: tail)
  | _ => none

def b64Decode (cs : List Char) : Option (List UInt8) :=
  decodeQuads (cs.filter fun c => c != '\r' && c != '\n')

def encodeL : List UInt8 → List Char
  | [] => []
  | [a] =>
    let x := a.toNat
    [b64Char (x / 4), b64Char ((x % 4) * 16), '=', '=']
  | [a, b] =>
    let x := a.toNat; let y := b.toNat
    [b64Char (x / 4), b64Char ((x % 4) * 16 + y / 16), b64Char ((y % 16) * 4), '=']
  | a :: b :: c :: rest =>
    let x := a.toNat; let y := b.toNat; let z := c.toNat
    b64Char (x / 4) :: b64Char ((x % 4) * 16 + y / 16) :: b64Char ((y % 16) * 4 + z / 64) ::
      b64Char (z % 64) :: encodeL rest

def b64Encode (s : String) : String := String.ofList (encodeL s.toUTF8.toList)

/-! ## Basic credentials -/

/-- If `v` is `Basic <base64>` (scheme case-insensitive) whose payload decodes to valid
UTF-8, the decoded `user:pass`. -/
def basicPayload (v : String) : Option String :=
  let cs := v.toList
  if cs.length > 6 && lowerAscii (String.ofList (cs.take 6)) == "basic " then
    match b64Decode (trimOWS (cs.drop 6)) with
    | some bytes => String.fromUTF8? (ByteArray.mk bytes.toArray)
    | none => none
  else none

/-- `value` appears in a header value, directly or inside a Basic credential. -/
def mentions (value v : String) : Bool :=
  contains v value || match basicPayload v with
    | some d => contains d value
    | none => false

/-- The request already carries the real value in a header, directly or inside a decoded
Basic credential. The empty value carries nowhere: every string contains it. -/
def carries (value v : String) : Bool := !value.isEmpty && mentions value v

/-- Swap placeholder `p` with `value` in one header value. Inside a Basic credential the
placeholder may be the user or the password; the credential is re-encoded. -/
def substitute (p value v : String) : String :=
  match basicPayload v with
  | some d =>
    if contains d p then String.ofList (v.toList.take 6) ++ b64Encode (replaceAll d p value)
    else replaceAll v p value
  | none => replaceAll v p value

/-! ## Rules -/

def lookup (rules : Rules) (name : String) : Option SecretRule :=
  rules.secrets.find? (·.name == name)

def allowedHeaders (sr : SecretRule) : List String :=
  (sr.headers.getD ["authorization"]).map lowerAscii

/-- Exact names, or `*.suffix` patterns matching one or more labels before `suffix`.
Both sides are compared ASCII-lower-cased. -/
def hostBound (hosts : List String) (host : String) : Bool :=
  let pats := hosts.map lowerAscii
  let rec go : List Char → Bool
    | [] => false
    | '.' :: rest => pats.contains ("*." ++ String.ofList rest) || go rest
    | _ :: rest => go rest
  let h := lowerAscii host
  pats.contains h || go h.toList

def allowOK : Option (List Allow) → Request → Bool
  | none, _ => true
  | some l, r => l.any fun a => a.method == r.method && a.pathPrefix.toList.isPrefixOf r.path.toList

/-- The header names a service may read the request's method from instead of the method on
the request line: `X-HTTP-Method-Override` is the one Google's APIs honour, and
`X-HTTP-Method` and `X-Method-Override` are the two other spellings in the wild. They are
held ASCII-lower-cased, which is how every header name here is compared. -/
def methodOverrideHeaders : List String :=
  ["x-http-method-override", "x-http-method", "x-method-override"]

/-- The request carries one of those headers, whatever its value: the value is the service's
to read, so the airlock refuses the header rather than deciding which methods one of them
could have named. -/
def hasMethodOverride (r : Request) : Bool :=
  r.headers.any fun h => methodOverrideHeaders.contains (lowerAscii h.1)

/-- The path holds a segment that is `.` or `..`: the path split on `"/"`, each segment
compared whole, so `"/v1/../admin"` and `"/a/./b"` hold one while `"/v1/..foo"` and
`"/v1/.../x"` hold none.

`String.splitOn "/"` and Go's `strings.Split(path, "/")` answer alike on every input - the
empty path is one empty segment on both sides, and a leading or trailing slash is an empty
segment at that end on both - so the two sides refuse the same paths. -/
def hasDotSegment (path : String) : Bool :=
  (path.splitOn "/").any fun s => s == "." || s == ".."

def namesUnique : List String → Bool
  | [] => true
  | n :: ns => !ns.contains n && namesUnique ns

/-- A judge asks for at most 900 seconds; an entry with no judge asks for nothing. -/
def judgeOK : Option Nat → Bool
  | none => true
  | some l => decide (l ≤ 900)

/-- The three kinds an entry may name. -/
def kindOK (k : String) : Bool := k == "" || k == "aws" || k == "azure"

/-- Rules-file validation. This is the same answer as Go's `Validate` returning `nil`:
the names are distinct, **every** entry binds at least one host (not only a judged one),
a judge lifetime is at most 900 s, and a kind is `""`, `"aws"` or `"azure"`. -/
def validate (rules : Rules) : Bool :=
  namesUnique (rules.secrets.map (·.name)) &&
  rules.secrets.all fun s => !s.hosts.isEmpty && judgeOK s.judge && kindOK s.kind

/-! ## Decision and rewrite -/

/-- The one decision both `decideReq` and `rewriteReq` take: the entry the request may
carry, or the reason it may not. The reasons and the order they are reported in are the
contract with Go's `check`.

Two of the eleven refusals are about the request rather than about the entry. The CONNECT
port must be 443: every name the decision compares - the CONNECT host, the TLS name and the
Host header - carries no port, so 443 is the only port the rules speak about, and another
port of one name is another service. It is read above the two name comparisons because the
port is part of what the CONNECT named.

A path that holds a dot segment is refused whatever the entry's `allow` says, an entry with
no `allow` included: an entry's path prefix bounds the path the service will read, and a
reader that resolves dot segments first - which is what RFC 3986 section 5.2.4 says a path
means - reads `"/v1/../admin"` as `"/admin"`, outside a `"/v1/"` prefix the literal path is
inside. A path two readers resolve differently is a path neither can be held to, so it is
refused rather than resolved here, which keeps one path in the decision and in what is
forwarded, and lets this model prove the bound in a line where `remove_dot_segments` would
be a page. It is read directly above the `allow` test because it is what makes that rule
mean what it says.

A request carrying a method override header is refused when, and only when, the entry has an
`allow` list. `allowOK` compares the method on the request line, while a service that reads
`X-HTTP-Method-Override` (Google's APIs do) or one of its two other spellings acts on the
method that header names, so an entry allowing `{POST, /prefix}` would carry the value on a
PATCH or a DELETE under that prefix. The header is refused rather than the method it names,
because which services read it, and which of them read which spelling, is not something the
rules file says. An entry with no `allow` list allows every method already, so such a header
takes nothing away from it and is not refused. It is read directly below the `allow` test,
for the same reason the dot segment is read directly above it: it is what makes that rule
mean what it says.

The last test reads **every** header of the request, not only the ones the entry names,
and an entry with a `kind` is not exempt: a request that already carries the real value
learnt it somewhere other than this swap, so the airlock refuses it. -/
def check (rules : Rules) (g : Grant) (value : String) (r : Request) :
    Except String SecretRule :=
  if g.placeholder.isEmpty then .error "empty placeholder" else
  match lookup rules g.secret with
  | none => .error "unknown secret"
  | some sr =>
    if sr.hosts.isEmpty then .error "secret not behind airlock"
    else if !hostBound sr.hosts r.connectHost then .error "host not bound"
    else if r.connectPort != 443 then .error "connect port is not 443"
    else if r.sni != r.connectHost then .error "sni mismatch"
    else if r.hostHeader != r.connectHost then .error "host header mismatch"
    else if hasDotSegment r.path then .error "path has a dot segment"
    else if !allowOK sr.allow r then .error "not allowed by rule"
    else if sr.allow.isSome && hasMethodOverride r then
      .error "request carries a method override header"
    else if r.headers.any (fun h => carries value h.2) then
      .error "request already carries the value"
    else .ok sr

def decideReq (rules : Rules) (g : Grant) (value : String) (r : Request) : Decision :=
  match check rules g value r with
  | .ok _ => .allow
  | .error e => .refuse e

def swapHeader (allowed : List String) (p value : String) (h : String × String) : String × String :=
  if allowed.contains (lowerAscii h.1) then (h.1, substitute p value h.2) else h

def rewriteReq (rules : Rules) (g : Grant) (value : String) (r : Request) : Option Request :=
  match check rules g value r with
  | .ok sr => some { r with headers := r.headers.map (swapHeader (allowedHeaders sr) g.placeholder value) }
  | .error _ => none

end Airlock
