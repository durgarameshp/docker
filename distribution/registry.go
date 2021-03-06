package distribution

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution"
	"github.com/docker/distribution/digest"
	"github.com/docker/distribution/manifest/schema1"
	"github.com/docker/distribution/registry/client"
	"github.com/docker/distribution/registry/client/auth"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/docker/docker/cliconfig"
	"github.com/docker/docker/registry"
	"golang.org/x/net/context"
)

type dumbCredentialStore struct {
	auth *cliconfig.AuthConfig
}

func (dcs dumbCredentialStore) Basic(*url.URL) (string, string) {
	return dcs.auth.Username, dcs.auth.Password
}

// NewV2Repository returns a repository (v2 only). It creates a HTTP transport
// providing timeout settings and authentication support, and also verifies the
// remote API version.
func NewV2Repository(repoInfo *registry.RepositoryInfo, endpoint registry.APIEndpoint, metaHeaders http.Header, authConfig *cliconfig.AuthConfig, actions ...string) (distribution.Repository, error) {
	ctx := context.Background()

	repoName := repoInfo.CanonicalName
	// If endpoint does not support CanonicalName, use the RemoteName instead
	if endpoint.TrimHostname {
		repoName = repoInfo.RemoteName
	}

	// TODO(dmcgowan): Call close idle connections when complete, use keep alive
	base := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     endpoint.TLSConfig,
		// TODO(dmcgowan): Call close idle connections when complete and use keep alive
		DisableKeepAlives: true,
	}

	modifiers := registry.DockerHeaders(metaHeaders)
	authTransport := transport.NewTransport(base, modifiers...)
	pingClient := &http.Client{
		Transport: authTransport,
		Timeout:   5 * time.Second,
	}
	endpointStr := strings.TrimRight(endpoint.URL, "/") + "/v2/"
	req, err := http.NewRequest("GET", endpointStr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := pingClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	versions := auth.APIVersions(resp, endpoint.VersionHeader)
	if endpoint.VersionHeader != "" && len(endpoint.Versions) > 0 {
		var foundVersion bool
		for _, version := range endpoint.Versions {
			for _, pingVersion := range versions {
				if version == pingVersion {
					foundVersion = true
				}
			}
		}
		if !foundVersion {
			return nil, errors.New("endpoint does not support v2 API")
		}
	}

	challengeManager := auth.NewSimpleChallengeManager()
	if err := challengeManager.AddResponse(resp); err != nil {
		return nil, err
	}

	if authConfig.RegistryToken != "" {
		passThruTokenHandler := &existingTokenHandler{token: authConfig.RegistryToken}
		modifiers = append(modifiers, auth.NewAuthorizer(challengeManager, passThruTokenHandler))
	} else {
		creds := dumbCredentialStore{auth: authConfig}
		tokenHandler := auth.NewTokenHandler(authTransport, creds, repoName.Name(), actions...)
		basicHandler := auth.NewBasicHandler(creds)
		modifiers = append(modifiers, auth.NewAuthorizer(challengeManager, tokenHandler, basicHandler))
	}
	tr := transport.NewTransport(base, modifiers...)

	return client.NewRepository(ctx, repoName.Name(), endpoint.URL, tr)
}

func digestFromManifest(m *schema1.SignedManifest, localName string) (digest.Digest, int, error) {
	payload, err := m.Payload()
	if err != nil {
		// If this failed, the signatures section was corrupted
		// or missing. Treat the entire manifest as the payload.
		payload = m.Raw
	}
	manifestDigest, err := digest.FromBytes(payload)
	if err != nil {
		logrus.Infof("Could not compute manifest digest for %s:%s : %v", localName, m.Tag, err)
	}
	return manifestDigest, len(payload), nil
}

type existingTokenHandler struct {
	token string
}

func (th *existingTokenHandler) Scheme() string {
	return "bearer"
}

func (th *existingTokenHandler) AuthorizeRequest(req *http.Request, params map[string]string) error {
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", th.token))
	return nil
}
