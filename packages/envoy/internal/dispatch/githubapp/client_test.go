package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

func testKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, string(encoded)
}

// fakeGitHub is an httptest GitHub App API: one installation per seeded
// repository, token minting, the repository read the minted token proves, and
// the commit/tree/blob reads the architecture importer walks.
type fakeGitHub struct {
	t *testing.T
	// installations maps "owner/repo" to its Contents permission; a missing
	// repo answers 404.
	installations  map[string]string
	installationID int64
	tokenMints     int
	tokenExpiresAt time.Time
	publicKey      *rsa.PublicKey
	clientID       string
	// commits maps a branch name to its commit sha; a missing branch answers 404.
	commits map[string]string
	// files maps a repository path to its content; a subtree read lists the
	// paths directly inside the requested directory and each blob is served
	// under sha "blob-<path>".
	files map[string]string
	// modes overrides an entry's mode (default 100644): 120000 is a symlink.
	modes map[string]string
	// sizes overrides an entry's listed size (default the content length).
	sizes map[string]int64
	// repoEntries pads the whole-repository tree (a read without ":dir") with
	// this many entries outside the architecture directory.
	repoEntries int
	// truncated makes the tree read answer truncated=true.
	truncated bool
	// missingBlobs answer 404 by path, simulating a blob vanishing mid-fetch.
	missingBlobs map[string]bool
	// treeReads counts tree reads, by "sha:dir".
	treeReads []string
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		f.verifyAppJWT(r)
		contents, ok := f.installations[r.PathValue("owner")+"/"+r.PathValue("repo")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		response := map[string]any{
			"id":          f.installationID,
			"app_slug":    "dispatch-test",
			"permissions": map[string]string{"contents": contents},
		}
		if contents == "" {
			response["permissions"] = map[string]string{}
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			f.t.Errorf("encode installation: %v", err)
		}
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.verifyAppJWT(r)
		f.tokenMints++
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_fake_%d_%d", f.installationID, f.tokenMints),
			"expires_at": f.tokenExpiresAt.Format(time.RFC3339),
		}); err != nil {
			f.t.Errorf("encode token: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_fake_") {
			f.t.Errorf("repository read used %q, want an installation token", r.Header.Get("Authorization"))
		}
		if _, ok := f.installations[r.PathValue("owner")+"/"+r.PathValue("repo")]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"full_name":%q}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	requireInstallationToken := func(r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_fake_") {
			f.t.Errorf("%s used %q, want an installation token", r.URL.Path, r.Header.Get("Authorization"))
		}
	}
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		requireInstallationToken(r)
		sha, ok := f.commits[r.PathValue("branch")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"sha":%q}`, sha)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, r *http.Request) {
		requireInstallationToken(r)
		if r.URL.RawQuery != "" {
			f.t.Errorf("tree read carries a query: %s", r.URL.RawQuery)
		}
		ref := r.PathValue("ref")
		f.treeReads = append(f.treeReads, ref)
		sha, dir, ok := strings.Cut(ref, ":")
		if !ok {
			// The whole-repository tree, GitHub-shaped (~258 bytes an entry):
			// repoEntries of padding plus every seeded file. An importer never
			// asks for it; the size is what a recursive read would have paid.
			fmt.Fprintf(w, `{"sha":%q,"truncated":%t,"tree":[`, "tree-"+sha, f.truncated)
			for i := range f.repoEntries {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				blob := fmt.Sprintf("%040d", i)
				fmt.Fprintf(w, `{"path":"packages/dispatch/web/src/features/issue/file-%d.tsx","mode":"100644","type":"blob","sha":%q,"size":%d,"url":"https://api.github.com/repos/legion/arch/git/blobs/%s"}`, i, blob, 7*i, blob)
			}
			for path := range f.files {
				fmt.Fprintf(w, `,{"path":%q,"mode":"100644","type":"blob","sha":%q,"size":%d}`, path, "blob-"+path, len(f.files[path]))
			}
			fmt.Fprint(w, `]}`)
			return
		}
		entries := []map[string]any{}
		prefix := dir + "/"
		names := []string{}
		for path := range f.files {
			if strings.HasPrefix(path, prefix) {
				names = append(names, strings.TrimPrefix(path, prefix))
			}
		}
		if len(names) == 0 {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		sort.Strings(names)
		digest := sha256.New()
		seenDirs := map[string]bool{}
		for _, name := range names {
			path := prefix + name
			if first, _, nested := strings.Cut(name, "/"); nested {
				// A nested path shows up as its top-level directory entry once.
				if !seenDirs[first] {
					seenDirs[first] = true
					entries = append(entries, map[string]any{
						"path": first, "mode": "040000", "type": "tree", "sha": "tree-" + prefix + first,
					})
				}
				continue
			}
			mode := f.modes[path]
			if mode == "" {
				mode = "100644"
			}
			size, ok := f.sizes[path]
			if !ok {
				size = int64(len(f.files[path]))
			}
			fmt.Fprintf(digest, "%s %s %s\n", mode, name, f.files[path])
			entries = append(entries, map[string]any{
				"path": name, "mode": mode, "type": "blob", "sha": "blob-" + path, "size": size,
			})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"sha": "tree-" + sha + "-" + fmt.Sprintf("%x", digest.Sum(nil))[:12], "truncated": f.truncated, "tree": entries,
		}); err != nil {
			f.t.Errorf("encode tree: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		requireInstallationToken(r)
		path := strings.TrimPrefix(r.PathValue("sha"), "blob-")
		content, ok := f.files[path]
		if !ok || f.missingBlobs[path] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// GitHub wraps blob base64 at 60 columns.
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		var wrapped strings.Builder
		for len(encoded) > 60 {
			wrapped.WriteString(encoded[:60] + "\n")
			encoded = encoded[60:]
		}
		wrapped.WriteString(encoded + "\n")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"content": wrapped.String(), "encoding": "base64", "size": len(content),
		}); err != nil {
			f.t.Errorf("encode blob: %v", err)
		}
	})
	return mux
}

// verifyAppJWT checks the Authorization JWT verifies against the fixture
// public key and carries the claims GitHub requires.
func (f *fakeGitHub) verifyAppJWT(r *http.Request) {
	f.t.Helper()
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		f.t.Errorf("app endpoint called with Authorization %q, want a bearer JWT", r.Header.Get("Authorization"))
		return
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		f.t.Errorf("JWT has %d segments, want 3", len(parts))
		return
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		f.t.Errorf("decode JWT header: %v", err)
		return
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "RS256" || header.Typ != "JWT" {
		f.t.Errorf("JWT header %s, want RS256/JWT", headerJSON)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		f.t.Errorf("decode JWT signature: %v", err)
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.publicKey, crypto.SHA256, digest[:], signature); err != nil {
		f.t.Errorf("JWT signature does not verify against the fixture key: %v", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		f.t.Errorf("decode JWT claims: %v", err)
		return
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		f.t.Errorf("parse JWT claims: %v", err)
		return
	}
	if claims.Iss != f.clientID {
		f.t.Errorf("JWT iss %q, want client ID %q", claims.Iss, f.clientID)
	}
	now := time.Now().Unix()
	if claims.Iat > now-30 {
		f.t.Errorf("JWT iat %d is not backdated (now %d)", claims.Iat, now)
	}
	if claims.Exp <= now || claims.Exp > now+10*60 {
		f.t.Errorf("JWT exp %d outside (now, now+10m] (now %d)", claims.Exp, now)
	}
}

func newTestClient(t *testing.T, fake *fakeGitHub) *Client {
	t.Helper()
	key, pemText := testKeyPEM(t)
	fake.publicKey = &key.PublicKey
	fake.clientID = "Iv1.testclient"
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", ClientSecret: "secret", PEM: pemText}, server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if client == nil {
		t.Fatal("new client: got nil for a configured app")
	}
	return client
}

func TestCheckSourceVerifiesJWTAndResolvesInstallation(t *testing.T) {
	fake := &fakeGitHub{
		t:              t,
		installations:  map[string]string{"legion/arch": "write"},
		installationID: 4242,
		tokenExpiresAt: time.Now().Add(time.Hour),
	}
	client := newTestClient(t, fake)
	source, err := client.CheckSource(context.Background(), "legion", "arch")
	if err != nil {
		t.Fatalf("check source: %v", err)
	}
	if source.InstallationID != 4242 || source.AppSlug != "dispatch-test" {
		t.Fatalf("source: got %+v", source)
	}
}

func TestInstallationMissingIsErrNoInstallation(t *testing.T) {
	fake := &fakeGitHub{t: t, installations: map[string]string{}, tokenExpiresAt: time.Now().Add(time.Hour)}
	client := newTestClient(t, fake)
	_, err := client.CheckSource(context.Background(), "legion", "missing")
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("missing installation: got %v, want ErrNoInstallation", err)
	}
	if !strings.Contains(err.Error(), "legion/missing") {
		t.Fatalf("error does not name the repository: %v", err)
	}
}

func TestContentsPermissionGatesTheCheck(t *testing.T) {
	for _, test := range []struct {
		contents string
		wantErr  bool
	}{
		{contents: "read"},
		{contents: "write"},
		{contents: "none", wantErr: true},
		{contents: "", wantErr: true},
	} {
		t.Run("contents="+test.contents, func(t *testing.T) {
			fake := &fakeGitHub{
				t:              t,
				installations:  map[string]string{"legion/arch": test.contents},
				installationID: 7,
				tokenExpiresAt: time.Now().Add(time.Hour),
			}
			client := newTestClient(t, fake)
			_, err := client.CheckSource(context.Background(), "legion", "arch")
			if test.wantErr {
				if !errors.Is(err, ErrNoContentsRead) {
					t.Fatalf("contents %q: got %v, want ErrNoContentsRead", test.contents, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("contents %q: %v", test.contents, err)
			}
		})
	}
}

func TestTokenCachesUntilNearExpiry(t *testing.T) {
	fake := &fakeGitHub{
		t:              t,
		installations:  map[string]string{"legion/arch": "read"},
		installationID: 9,
		tokenExpiresAt: time.Now().Add(time.Hour),
	}
	client := newTestClient(t, fake)
	first, err := client.Token(context.Background(), 9)
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	second, err := client.Token(context.Background(), 9)
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if fake.tokenMints != 1 {
		t.Fatalf("token mints: got %d, want 1 (second call served from cache)", fake.tokenMints)
	}
	if first != second {
		t.Fatalf("cached token changed: %q then %q", first, second)
	}

	// Within five minutes of expiry the cache no longer serves the token: age the
	// cached entry rather than the clock so the re-mint's JWT stays verifiable.
	client.mu.Lock()
	client.tokens[9] = cachedToken{token: first, expires: client.now().Add(4 * time.Minute)}
	client.mu.Unlock()
	if _, err := client.Token(context.Background(), 9); err != nil {
		t.Fatalf("re-mint near expiry: %v", err)
	}
	if fake.tokenMints != 2 {
		t.Fatalf("token mints after expiry: got %d, want 2", fake.tokenMints)
	}
}

func TestNilClientAnswersErrNoAppKey(t *testing.T) {
	client, err := New(nil, "")
	if err != nil || client != nil {
		t.Fatalf("New(nil): got (%v, %v), want (nil, nil)", client, err)
	}
	client, err = New(&auth.AppConfig{ClientID: "Iv1.x", ClientSecret: "s"}, "")
	if err != nil || client != nil {
		t.Fatalf("New(no PEM): got (%v, %v), want (nil, nil)", client, err)
	}
	if _, err := client.CheckSource(context.Background(), "legion", "arch"); !errors.Is(err, ErrNoAppKey) {
		t.Fatalf("nil client check: got %v, want ErrNoAppKey", err)
	}
}

func TestNewRejectsMalformedKey(t *testing.T) {
	if _, err := New(&auth.AppConfig{ClientID: "Iv1.x", ClientSecret: "s", PEM: "not a key"}, ""); err == nil {
		t.Fatal("malformed PEM accepted")
	}
}

func newReaderFixture(t *testing.T, fake *fakeGitHub) (*Client, string) {
	t.Helper()
	fake.installations = map[string]string{"legion/arch": "read"}
	fake.installationID = 7
	fake.tokenExpiresAt = time.Now().Add(time.Hour)
	if fake.commits == nil {
		fake.commits = map[string]string{"main": "c0ffee"}
	}
	client := newTestClient(t, fake)
	token, err := client.Token(context.Background(), 7)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return client, token
}

func TestRefResolvesTheBranchCommit(t *testing.T) {
	fake := &fakeGitHub{t: t}
	client, token := newReaderFixture(t, fake)

	sha, err := client.Ref(context.Background(), token, "legion", "arch", "main")
	if err != nil || sha != "c0ffee" {
		t.Fatalf("ref: sha=%q err=%v", sha, err)
	}

	if _, err := client.Ref(context.Background(), token, "legion", "arch", "gone"); !errors.Is(err, ErrNoBranch) {
		t.Fatalf("missing branch: err=%v, want ErrNoBranch", err)
	}
}

// readDir lists then reads: the two calls a sync makes.
func readDir(t *testing.T, client *Client, token string) (Dir, map[string][]byte, error) {
	t.Helper()
	listing, err := client.Dir(context.Background(), token, "legion", "arch", "c0ffee", ".dispatch/architecture")
	if err != nil {
		return listing, nil, err
	}
	files, err := client.DirFiles(context.Background(), token, "legion", "arch", listing)
	return listing, files, err
}

func TestDirFilesReadsEveryRegularMarkdownFileInTheDirectory(t *testing.T) {
	fake := &fakeGitHub{
		t: t,
		files: map[string]string{
			".dispatch/architecture/api.md":      "the api\n",
			".dispatch/architecture/store.md":    "the store\n",
			".dispatch/architecture/tool.md":     "executable bit\n",
			".dispatch/architecture/link.md":     "../../README.md",
			".dispatch/architecture/notes.txt":   "not markdown",
			".dispatch/architecture/sub/deep.md": "not a direct child",
			"README.md":                          "not in the directory",
			".dispatch/architecture-adjacent.md": "prefix sibling, not inside the directory",
		},
		modes: map[string]string{
			".dispatch/architecture/tool.md": "100755",
			".dispatch/architecture/link.md": "120000",
		},
	}
	client, token := newReaderFixture(t, fake)

	listing, files, err := readDir(t, client, token)
	if err != nil {
		t.Fatalf("dir files: %v", err)
	}
	want := map[string][]byte{"api.md": []byte("the api\n"), "store.md": []byte("the store\n"), "tool.md": []byte("executable bit\n")}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %#v, want %#v (a symlink is not a component)", files, want)
	}
	if listing.SHA == "" {
		t.Fatal("listing carries no subtree sha")
	}
	if !reflect.DeepEqual(fake.treeReads, []string{"c0ffee:.dispatch/architecture"}) {
		t.Fatalf("tree reads = %#v, want the one subtree read", fake.treeReads)
	}
}

// The listing is the subtree alone: a repository whose recursive tree would
// blow past the 1 MiB response cap (4,000+ entries at GitHub's ~258 B each)
// still syncs, because the importer never asks for that tree.
func TestDirListsTheSubtreeNotTheRepository(t *testing.T) {
	fake := &fakeGitHub{
		t:           t,
		files:       map[string]string{".dispatch/architecture/api.md": "the api\n"},
		repoEntries: 6000,
	}
	client, token := newReaderFixture(t, fake)

	// The whole-repository tree this fake would answer does not fit the cap.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/repos/legion/arch/git/trees/c0ffee", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	fake.handler().ServeHTTP(recorder, request)
	if recorder.Body.Len() <= responseLimit {
		t.Fatalf("whole-repository tree is %d bytes, want more than the %d cap", recorder.Body.Len(), responseLimit)
	}
	fake.treeReads = nil

	_, files, err := readDir(t, client, token)
	if err != nil || len(files) != 1 {
		t.Fatalf("subtree read: files=%v err=%v", files, err)
	}
	if !reflect.DeepEqual(fake.treeReads, []string{"c0ffee:.dispatch/architecture"}) {
		t.Fatalf("tree reads = %#v, want only the subtree", fake.treeReads)
	}
}

func TestDirMissingDirectoryIsAnEmptyListing(t *testing.T) {
	fake := &fakeGitHub{t: t, files: map[string]string{"README.md": "no architecture dir"}}
	client, token := newReaderFixture(t, fake)

	listing, files, err := readDir(t, client, token)
	if err != nil || len(files) != 0 || listing.SHA != "" || len(listing.Files) != 0 {
		t.Fatalf("missing directory: listing=%+v files=%v err=%v, want empty and no error", listing, files, err)
	}
}

func TestDirTruncatedTreeIsTyped(t *testing.T) {
	fake := &fakeGitHub{t: t, truncated: true, files: map[string]string{".dispatch/architecture/api.md": "x"}}
	client, token := newReaderFixture(t, fake)

	if _, _, err := readDir(t, client, token); !errors.Is(err, ErrTreeTruncated) {
		t.Fatalf("truncated tree: err=%v, want ErrTreeTruncated", err)
	}
}

func TestDirFilesBlobVanishingMidFetchFailsTheWholeRead(t *testing.T) {
	fake := &fakeGitHub{
		t: t,
		files: map[string]string{
			".dispatch/architecture/api.md":   "the api\n",
			".dispatch/architecture/store.md": "the store\n",
		},
		missingBlobs: map[string]bool{".dispatch/architecture/api.md": true, ".dispatch/architecture/store.md": true},
	}
	client, token := newReaderFixture(t, fake)

	if _, _, err := readDir(t, client, token); err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("vanished blob: err=%v, want a 404 failure", err)
	}
}

// A file at the cap reads whole (base64 wrapping does not eat into the
// limit); one byte over is refused by its listed size before any blob read.
func TestDirFilesEnforceMaxFileSize(t *testing.T) {
	atCap := strings.Repeat("x", MaxFileSize)
	fake := &fakeGitHub{t: t, files: map[string]string{".dispatch/architecture/big.md": atCap}}
	client, token := newReaderFixture(t, fake)
	_, files, err := readDir(t, client, token)
	if err != nil || len(files["big.md"]) != MaxFileSize {
		t.Fatalf("file at the cap: len=%d err=%v", len(files["big.md"]), err)
	}

	fake.sizes = map[string]int64{".dispatch/architecture/big.md": MaxFileSize + 1}
	fake.missingBlobs = map[string]bool{".dispatch/architecture/big.md": true}
	_, _, err = readDir(t, client, token)
	if !errors.Is(err, ErrFileTooLarge) || !strings.Contains(err.Error(), ".dispatch/architecture/big.md") {
		t.Fatalf("oversized listing: err=%v, want ErrFileTooLarge naming the file, refused before the blob read", err)
	}
}
