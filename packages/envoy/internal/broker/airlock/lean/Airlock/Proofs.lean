import Airlock.Model

/-!
# Airlock proofs

The five theorems requirement R8 asks for. None of them puts a condition on the incoming
request: `check`'s own last refusal — a request that already carries the real value — is
what buys that, so a theorem about the rewrite needs no freshness hypothesis.
-/

namespace Airlock

/-- Everything `check` establishes when it accepts, including that no header of the
incoming request carries the value. -/
theorem check_ok {rules : Rules} {g : Grant} {value : String} {r : Request} {sr : SecretRule}
    (h : check rules g value r = .ok sr) :
    lookup rules g.secret = some sr ∧ sr.hosts ≠ [] ∧ hostBound sr.hosts r.connectHost = true ∧
      r.sni = r.connectHost ∧ r.hostHeader = r.connectHost ∧ allowOK sr.allow r = true ∧
      ∀ hd ∈ r.headers, carries value hd.2 = false := by
  unfold check at h
  split at h
  · cases h
  cases hl : lookup rules g.secret with
  | none => simp [hl] at h
  | some sr' =>
    simp only [hl] at h
    split at h
    · cases h
    split at h
    · cases h
    split at h
    · cases h
    split at h
    · cases h
    split at h
    · cases h
    split at h
    · cases h
    cases h
    simp_all
    assumption

/-- `rewriteReq` succeeds exactly when `decideReq` allows: a swap happens exactly when the
decision allows. -/
theorem rewrite_some_iff_allow (rules : Rules) (g : Grant) (value : String) (r : Request) :
    (rewriteReq rules g value r).isSome = (decideReq rules g value r == .allow) := by
  unfold rewriteReq decideReq
  split <;> rfl

/-- T1 (structural form): the rewrite keeps every header name and its position; a header
whose name the entry does not list is passed through byte for byte; the query and the body
are unchanged.

The statement holds for every entry, an entry with a `kind` included, so it holds in
particular for the entries without a kind that requirement R8 names. -/
theorem T1_structure {rules : Rules} {g : Grant} {value : String} {r r' : Request}
    (h : rewriteReq rules g value r = some r') :
    ∃ sr, lookup rules g.secret = some sr ∧
      r'.query = r.query ∧ r'.body = r.body ∧
      r'.headers.map Prod.fst = r.headers.map Prod.fst ∧
      ∀ h' ∈ r'.headers, ∃ h0 ∈ r.headers, h'.1 = h0.1 ∧
        ((allowedHeaders sr).contains (lowerAscii h0.1) = false → h' = h0) := by
  unfold rewriteReq at h
  split at h
  · rename_i sr hc
    cases h
    refine ⟨sr, (check_ok hc).1, rfl, rfl, ?_, ?_⟩
    · simp only [List.map_map]
      congr 1
      funext x
      simp only [Function.comp, swapHeader]
      split <;> rfl
    · intro h' hm
      obtain ⟨h0, hm0, rfl⟩ := List.mem_map.mp hm
      refine ⟨h0, hm0, ?_, ?_⟩
      · simp only [swapHeader]; split <;> rfl
      · intro hn
        simp only [swapHeader, hn, Bool.false_eq_true, ite_false]
  · cases h

/-- T1: when `rewriteReq` answers a request, its query and its body are the ones that came
in, and every header of the answer that `carries` the value has a name the entry's header
list holds. There is no hypothesis beyond the rewrite: `check` refuses a request that
already carries the value, and `carries` is false for the empty value, so neither a
freshness hypothesis nor a hypothesis about the value is needed.

The statement holds for every entry, an entry with a `kind` included, so it holds in
particular for the entries without a kind that requirement R8 names. -/
theorem T1 {rules : Rules} {g : Grant} {value : String} {r r' : Request}
    (h : rewriteReq rules g value r = some r') :
    ∃ sr, lookup rules g.secret = some sr ∧
      r'.query = r.query ∧ r'.body = r.body ∧
      ∀ hd ∈ r'.headers, carries value hd.2 = true →
        (allowedHeaders sr).contains (lowerAscii hd.1) = true := by
  have hfresh : ∀ hd ∈ r.headers, carries value hd.2 = false := by
    unfold rewriteReq at h
    split at h
    · rename_i sr' hc
      exact (check_ok hc).2.2.2.2.2.2
    · cases h
  obtain ⟨sr, hl, hq, hb, -, hh⟩ := T1_structure h
  refine ⟨sr, hl, hq, hb, ?_⟩
  intro hd hm hcar
  obtain ⟨h0, hm0, hname, hpass⟩ := hh hd hm
  cases hc : (allowedHeaders sr).contains (lowerAscii h0.1)
  · have heq := hpass hc
    subst heq
    rw [hfresh hd hm0] at hcar
    cases hcar
  · rw [hname]; exact hc

/-- T2: a rewrite happens only on a CONNECT host the entry binds, and only when the TLS
name and the Host header both equal that host.

The statement holds for every entry, an entry with a `kind` included, so it holds in
particular for the entries without a kind that requirement R8 names. -/
theorem T2 {rules : Rules} {g : Grant} {value : String} {r r' : Request}
    (h : rewriteReq rules g value r = some r') :
    ∃ sr, lookup rules g.secret = some sr ∧ hostBound sr.hosts r.connectHost = true ∧
      r.sni = r.connectHost ∧ r.hostHeader = r.connectHost ∧
      r'.connectHost = r.connectHost ∧ r'.sni = r.sni ∧ r'.hostHeader = r.hostHeader := by
  unfold rewriteReq at h
  split at h
  · rename_i sr hc
    cases h
    obtain ⟨hl, -, hb, hs, hh, -⟩ := check_ok hc
    exact ⟨sr, hl, hb, hs, hh, rfl, rfl, rfl⟩
  · cases h

/-- T3: a valid rules file gives every entry at least one host, and a judge a lifetime of
at most 900 seconds. -/
theorem T3 {rules : Rules} (hv : validate rules = true) :
    ∀ sr ∈ rules.secrets, sr.hosts ≠ [] ∧ ∀ l, sr.judge = some l → l ≤ 900 := by
  intro sr hm
  simp only [validate, Bool.and_eq_true, List.all_eq_true] at hv
  have hs := hv.2 sr hm
  simp only [Bool.not_eq_true', List.isEmpty_eq_false_iff] at hs
  refine ⟨hs.1.1, ?_⟩
  intro l hj
  have hjo := hs.1.2
  rw [hj] at hjo
  simpa [judgeOK] using hjo

end Airlock
