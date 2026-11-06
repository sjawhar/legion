import Lean.Data.Json
import Airlock

/-! Line protocol: one JSON case per stdin line, one JSON answer per stdout line.
case   = {"rules":{"secrets":[...]}, "grant":{...}, "value":"...", "request":{...}}
answer = {"valid":bool, "allow":bool, "reason":"...", "rewrite":request|null}
         or {"error":"..."} when the line does not parse.

The Go side marshals the same shapes with no `omitempty`, so an absent list reaches this
program as `null`, which reads as `none`, and an entry with no kind reaches it as `""`. -/

open Lean Airlock

deriving instance FromJson, ToJson for Allow
deriving instance FromJson, ToJson for SecretRule
deriving instance FromJson, ToJson for Rules
deriving instance FromJson, ToJson for Grant
deriving instance FromJson, ToJson for Request

structure Case where
  rules : Rules
  grant : Grant
  value : String
  request : Request
  deriving FromJson

def answer (c : Case) : Json :=
  let (allow, reason) := match decideReq c.rules c.grant c.value c.request with
    | .allow => (true, "")
    | .refuse e => (false, e)
  Json.mkObj [
    ("valid", toJson (validate c.rules)),
    ("allow", toJson allow),
    ("reason", toJson reason),
    ("rewrite", match rewriteReq c.rules c.grant c.value c.request with
      | some r => toJson r
      | none => Json.null)]

partial def loop (stdin : IO.FS.Stream) (stdout : IO.FS.Stream) : IO Unit := do
  let line ← stdin.getLine
  if line.isEmpty then return
  let out := match Json.parse line >>= fromJson? with
    | .ok (c : Case) => answer c
    | .error e => Json.mkObj [("error", toJson e)]
  stdout.putStrLn out.compress
  stdout.flush
  loop stdin stdout

def main : IO Unit := do
  loop (← IO.getStdin) (← IO.getStdout)
