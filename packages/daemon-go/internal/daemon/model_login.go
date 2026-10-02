package daemon

import (
	"log/slog"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/modellogin"
)

// newModelLogin builds the daemon-held Cognito login from the optional Kubernetes configuration.
// A credential-document failure becomes a stateful failed manager rather than a silent missing
// token, so the running daemon reports it through logs and GET /legion/v1/state.
func newModelLogin(cfg config.Config, log *slog.Logger) *modellogin.Manager {
	kubernetes := cfg.Runtime.Kubernetes
	if kubernetes == nil || kubernetes.ModelLogin == nil {
		return nil
	}
	configured := kubernetes.ModelLogin
	document, err := config.ExecuteKeyCommand(configured.LoginCommand, "runtime.kubernetes.model_login.login_command")
	if err != nil {
		return modellogin.Failed(err, log)
	}
	login, err := modellogin.ParseLoginDocument(document)
	if err != nil {
		return modellogin.Failed(err, log)
	}
	client := modellogin.NewCognitoClient(modellogin.CognitoConfig{
		Region:   login.Region,
		ClientID: login.ClientID,
	})
	return modellogin.New(modellogin.Credentials{Username: login.Username, Password: login.Password}, client, nil, log)
}
