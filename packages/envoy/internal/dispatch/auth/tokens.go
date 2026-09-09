package auth

// Tokens is the persisted GitHub OAuth state for the logged-in dashboard user.
type Tokens struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	AccessExpiresAt  int64  `json:"accessExpiresAt"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
	GithubLogin      string `json:"githubLogin"`
}
