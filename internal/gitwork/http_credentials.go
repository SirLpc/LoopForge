package gitwork

import (
	"context"
	"os"
	"strings"
)

// HTTPCredential authenticates exactly one HTTP(S) endpoint. The map containing it
// belongs to a live execution; it is never written to a checkout or git config.
type HTTPCredential struct {
	Username string
	Token    string
}

type httpCredentialsKey struct{}
type httpRemoteKey struct{}

// WithHTTPCredentials binds the only HTTP(S) credentials this execution may use.
// Empty and nil maps deliberately provide no credentials.
func WithHTTPCredentials(ctx context.Context, credentials map[string]HTTPCredential) context.Context {
	return context.WithValue(ctx, httpCredentialsKey{}, credentials)
}

// WithHTTPRemote identifies the repository for commands such as fetch origin,
// whose arguments do not contain its URL. Explicit URL arguments take priority.
func WithHTTPRemote(ctx context.Context, remote string) context.Context {
	return context.WithValue(ctx, httpRemoteKey{}, remote)
}

func httpCredentialForCommand(ctx context.Context, args []string) (credential HTTPCredential, scope, remote string) {
	credentials, scoped := ctx.Value(httpCredentialsKey{}).(map[string]HTTPCredential)
	if !scoped {
		return HTTPCredential{}, "", ""
	}
	remote, _ = ctx.Value(httpRemoteKey{}).(string)
	for _, arg := range args {
		if strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://") || IsSSHReference(arg) {
			remote = arg
			break
		}
	}
	scope, ok := HTTPRemoteScope(remote)
	if !ok {
		return HTTPCredential{}, "", ""
	}
	credential = credentials[scope]
	if credential.Username == "" {
		credential.Username = "x-access-token"
	}
	return credential, scope, remote
}

const gitHTTPHostEnv = "AGENTFLOW_GIT_HTTP_HOST"

// scopedCredentialHelper accepts Git's protocol, rather than returning a
// password to every host a redirect, submodule or checkout might request.
func scopedCredentialHelper() string {
	return `!f() {
		test "$1" = get || exit 0
		protocol= host=
		while IFS= read -r line && test -n "$line"; do
			case "$line" in
				protocol=*) protocol=${line#protocol=} ;;
				host=*) host=${line#host=} ;;
			esac
		done
		host=$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')
		case "$protocol" in
			https) scope=${host%:443} ;;
			http) scope=http://${host%:80} ;;
			*) exit 0 ;;
		esac
		test "$scope" = "$` + gitHTTPHostEnv + `" || exit 0
		printf 'username=%s\npassword=%s\n' "$` + gitHTTPUserEnv + `" "$` + gitHTTPPassEnv + `"
	}; f`
}

func scopedHTTPAuthArgs(credential HTTPCredential, scope, remote string) []string {
	args := []string{"-c", "credential.helper="}
	if credential.Token != "" {
		args = append(args, "-c", "credential.helper="+scopedCredentialHelper())
	}
	if strings.HasPrefix(scope, PlainHTTPPrefix) {
		args = append(args, "-c", "http.followRedirects=false", "-c", "http.proxy=", "-c", "remote.origin.proxy=")
		if exact, _, err := HTTPRepositoryURL(remote); err == nil {
			args = append(args, "-c", "http."+exact+".followRedirects=false", "-c", "http."+exact+".proxy=")
		}
	}
	return args
}

func privateHTTPHome(ctx context.Context) (string, func(), error) {
	if UsesSSH(ctx) {
		return "", func() {}, nil
	}
	home, err := os.MkdirTemp("", "loopforge-git-home-")
	return home, func() { _ = os.RemoveAll(home) }, err
}

func redactContextGitSecrets(ctx context.Context, text string) string {
	credentials, _ := ctx.Value(httpCredentialsKey{}).(map[string]HTTPCredential)
	for _, credential := range credentials {
		text = Redact(text, credential.Token)
	}
	return text
}
