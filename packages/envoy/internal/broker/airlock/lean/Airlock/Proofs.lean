import Airlock.Model

/-!
# Airlock proofs

The five theorems requirement R8 asks for — `T1`, `T1_structure`, `T2`, `T3` and
`rewrite_some_iff_allow`, the five `Axioms.lean` prints and check k15 names. None of them puts
a condition on the incoming request: `check`'s own last refusal — a request that already
carries the real value — is what buys that, so a theorem about the rewrite needs no freshness
hypothesis. `check_ok`, `swapHeader_fst` and `swapHeader_of_not_allowed` are the three lemmas
those five are read through, and are no part of what R8 asks for.
-/

namespace Airlock

/-- Everything `check` establishes when it accepts: the entry, that it binds the CONNECT
host, that the CONNECT port is 443, that the TLS name and the Host header are that host,
that the path holds no dot segment, that the entry's `allow` covers the request, that an
entry with an `allow` list is not handed a method override header, and that no header of
the incoming request carries the value. -/
theorem check_ok {rules : Rules} {g : Grant} {value : String} {r : Request} {sr : SecretRule}
    (h : check rules g value r = .ok sr) :
    lookup rules g.secret = some sr ∧ sr.hosts ≠ [] ∧ hostBound sr.hosts r.connectHost = true ∧
      r.connectPort = 443 ∧ r.sni = r.connectHost ∧ r.hostHeader = r.connectHost ∧
      hasDotSegment r.path = false ∧ allowOK sr.allow r = true ∧
      (sr.allow.isSome && hasMethodOverride r) = false ∧
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

/-- The swap never touches a header's name, whichever branch it takes. -/
theorem swapHeader_fst (allowed : List String) (p value : String) (h : String × String) :
    (swapHeader allowed p value h).1 = h.1 := by
  simp only [swapHeader]; split <;> rfl

/-- A header whose name the entry does not list comes out of the swap as it went in. -/
theorem swapHeader_of_not_allowed {allowed : List String} {p value : String}
    {h : String × String} (hn : allowed.contains (lowerAscii h.1) = false) :
    swapHeader allowed p value h = h := by
  simp only [swapHeader, hn, Bool.false_eq_true, ite_false]

/-- T1 (structural form): the rewrite answers the request that came in, header for header,
at every position. The method, the query and the body are the ones that came in; the answer
has as many headers as the request; the header at each position has the name the request's
header at that position had; and where that name is not one the entry lists, the whole pair
is the pair that came in, byte for byte.

Every clause is pinned by position, not by "some input header of the same name": a rewrite
that dropped, repeated or reordered the headers of one name, or that appended anything to
the method, would answer a request this statement does not describe, which is what
requirement R8 and check k15 say the proofs hold the rewrite to.

The statement holds for every entry, an entry with a `kind` included, so it holds in
particular for the entries without a kind that requirement R8 names. -/
theorem T1_structure {rules : Rules} {g : Grant} {value : String} {r r' : Request}
    (h : rewriteReq rules g value r = some r') :
    ∃ sr, lookup rules g.secret = some sr ∧
      r'.query = r.query ∧ r'.body = r.body ∧ r'.method = r.method ∧
      r'.headers.length = r.headers.length ∧
      ∀ (i : Nat) (hi : i < r.headers.length) (hi' : i < r'.headers.length),
        r'.headers[i].1 = r.headers[i].1 ∧
          ((allowedHeaders sr).contains (lowerAscii r.headers[i].1) = false →
            r'.headers[i] = r.headers[i]) := by
  unfold rewriteReq at h
  split at h
  · rename_i sr hc
    cases h
    refine ⟨sr, (check_ok hc).1, rfl, rfl, rfl, by simp, ?_⟩
    intro i hi hi'
    simp only [List.getElem_map]
    exact ⟨swapHeader_fst _ _ _ _, fun hn => swapHeader_of_not_allowed hn⟩
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
  unfold rewriteReq at h
  split at h
  · rename_i sr hc
    cases h
    have hfresh : ∀ hd ∈ r.headers, carries value hd.2 = false := (check_ok hc).2.2.2.2.2.2.2.2.2
    refine ⟨sr, (check_ok hc).1, rfl, rfl, ?_⟩
    intro hd hm hcar
    obtain ⟨h0, hm0, rfl⟩ := List.mem_map.mp hm
    rw [swapHeader_fst]
    cases hc : (allowedHeaders sr).contains (lowerAscii h0.1)
    · rw [swapHeader_of_not_allowed hc, hfresh h0 hm0] at hcar
      cases hcar
    · rfl
  · cases h

/-- T2: a rewrite happens only on a CONNECT host the entry binds, only on port 443 of that
host, only when the TLS name and the Host header both equal that host, only on a path that
holds no dot segment, and, for an entry with an `allow` list, only on a request that carries
no method override header.

The port is part of the statement because every name compared here carries none, so 443 is
the only port the rules speak about and another port of one name is another service. The
path is part of it because the entry's `allow` prefix is a bound on the path the service
will read: a path with a dot segment is one a reader resolves into another path
(RFC 3986 section 5.2.4), so the airlock carries none, and the path the swap answers is the
path that was decided.

The method override header is part of it for the same reason: `allowOK` reads the method on
the request line, and a service that reads `X-HTTP-Method-Override` or one of its two other
spellings acts on another method, so an entry's `allow` would name one method and the
service read another. An entry with no `allow` list allows every method already, which is
why the clause is conditional on the list being there.

The statement holds for every entry, an entry with a `kind` included, so it holds in
particular for the entries without a kind that requirement R8 names. -/
theorem T2 {rules : Rules} {g : Grant} {value : String} {r r' : Request}
    (h : rewriteReq rules g value r = some r') :
    ∃ sr, lookup rules g.secret = some sr ∧ hostBound sr.hosts r.connectHost = true ∧
      r.connectPort = 443 ∧ r.sni = r.connectHost ∧ r.hostHeader = r.connectHost ∧
      hasDotSegment r.path = false ∧ (sr.allow.isSome && hasMethodOverride r) = false ∧
      r'.connectHost = r.connectHost ∧ r'.connectPort = r.connectPort ∧ r'.sni = r.sni ∧
      r'.hostHeader = r.hostHeader ∧ r'.path = r.path := by
  unfold rewriteReq at h
  split at h
  · rename_i sr hc
    cases h
    obtain ⟨hl, -, hb, hp, hs, hh, hd, -, hm, -⟩ := check_ok hc
    exact ⟨sr, hl, hb, hp, hs, hh, hd, hm, rfl, rfl, rfl, rfl, rfl⟩
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
