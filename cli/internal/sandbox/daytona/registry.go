package daytona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// A Daytona snapshot pulled from an image runs only the image's ENTRYPOINT (the runner reads
// Config.Entrypoint and drops Config.Cmd), so an image such as redis or postgres, whose
// entrypoint script needs its CMD, would start nothing. The provider therefore reads the
// image's config from its registry (OCI distribution API, anonymous pull token) and passes
// ENTRYPOINT followed by CMD as the snapshot's entrypoint.

var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// imageRef is a parsed image name.
type imageRef struct {
	Domain    string // registry host, e.g. registry-1.docker.io
	Repo      string // e.g. library/redis
	Reference string // a tag or a digest
}

func parseImage(image string) (imageRef, error) {
	if image == "" || strings.ContainsAny(image, " \t\r\n") {
		return imageRef{}, fmt.Errorf("the image %q is not a valid image name", image)
	}
	name, ref := image, "latest"
	if i := strings.Index(name, "@"); i >= 0 {
		name, ref = name[:i], name[i+1:]
	} else if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, ref = name[:i], name[i+1:]
	}
	domain := "docker.io"
	if i := strings.Index(name, "/"); i >= 0 && (strings.ContainsAny(name[:i], ".:") || name[:i] == "localhost") {
		domain, name = name[:i], name[i+1:]
	}
	if domain == "docker.io" || domain == "index.docker.io" {
		domain = "registry-1.docker.io"
		if !strings.Contains(name, "/") {
			name = "library/" + name
		}
	}
	if name == "" || ref == "" {
		return imageRef{}, fmt.Errorf("the image %q is not a valid image name", image)
	}
	return imageRef{Domain: domain, Repo: name, Reference: ref}, nil
}

// imageCommand returns the image's ENTRYPOINT followed by its CMD, for linux/amd64 (the
// platform Daytona's runners use).
func (p *Provider) imageCommand(ctx context.Context, image string) ([]string, error) {
	ref, err := parseImage(image)
	if err != nil {
		return nil, err
	}
	r := &registry{p: p, ref: ref}
	manifest, err := r.manifest(ctx, ref.Reference)
	if err != nil {
		return nil, err
	}
	if len(manifest.Manifests) > 0 {
		digest := ""
		for _, m := range manifest.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
				digest = m.Digest
				break
			}
		}
		if digest == "" {
			return nil, fmt.Errorf("the image %s has no linux/amd64 variant, which Daytona runs", image)
		}
		if manifest, err = r.manifest(ctx, digest); err != nil {
			return nil, err
		}
	}
	if manifest.Config.Digest == "" {
		return nil, fmt.Errorf("the registry sent no image config for %s", image)
	}
	var config struct {
		Config struct {
			Entrypoint []string `json:"Entrypoint"`
			Cmd        []string `json:"Cmd"`
		} `json:"config"`
	}
	if err := r.getJSON(ctx, "/blobs/"+manifest.Config.Digest, "", &config); err != nil {
		return nil, fmt.Errorf("read the image config of %s: %w", image, err)
	}
	command := append(append([]string(nil), config.Config.Entrypoint...), config.Config.Cmd...)
	if len(command) == 0 {
		return nil, fmt.Errorf("the service image %s has no ENTRYPOINT or CMD to run", image)
	}
	return command, nil
}

type manifestDoc struct {
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

// registry reads one repository with an anonymous bearer token, fetched on the first 401.
type registry struct {
	p     *Provider
	ref   imageRef
	token string
}

func (r *registry) manifest(ctx context.Context, reference string) (manifestDoc, error) {
	var m manifestDoc
	if err := r.getJSON(ctx, "/manifests/"+reference, manifestTypes, &m); err != nil {
		return m, fmt.Errorf("read the manifest of %s/%s:%s: %w", r.ref.Domain, r.ref.Repo, reference, err)
	}
	return m, nil
}

func (r *registry) getJSON(ctx context.Context, suffix, accept string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	target := r.p.registry(r.ref.Domain) + "/v2/" + r.ref.Repo + suffix
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if r.token != "" {
			req.Header.Set("Authorization", "Bearer "+r.token)
		}
		resp, err := r.p.c.http.Do(req)
		if err != nil {
			return fmt.Errorf("reach the registry %s: %w", r.ref.Domain, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := resp.Header.Get("Www-Authenticate")
			resp.Body.Close()
			if err := r.authorize(ctx, challenge); err != nil {
				return err
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			err := errorOf(resp)
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return fmt.Errorf("the registry %s refused an anonymous pull; the Daytona provider supports public service images only: %w", r.ref.Domain, err)
			}
			return err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return err
		}
		return json.Unmarshal(data, out)
	}
}

// authorize fetches an anonymous pull token for the challenge of a 401.
func (r *registry) authorize(ctx context.Context, challenge string) error {
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") || params["realm"] == "" {
		return fmt.Errorf("the registry %s asks for %q authentication; the Daytona provider supports public service images only", r.ref.Domain, challenge)
	}
	q := url.Values{}
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + r.ref.Repo + ":pull"
	}
	q.Set("scope", scope)
	realm, err := url.Parse(params["realm"])
	if err != nil {
		return fmt.Errorf("the registry %s sent a bad token realm: %w", r.ref.Domain, err)
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	resp, err := r.p.c.http.Do(req)
	if err != nil {
		return fmt.Errorf("get a pull token from %s: %w", realm.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("get a pull token from %s: %w", realm.Host, errorOf(resp))
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return fmt.Errorf("read the pull token from %s: %w", realm.Host, err)
	}
	r.token = body.Token
	if r.token == "" {
		r.token = body.AccessToken
	}
	if r.token == "" {
		return errors.New("the registry token service sent no token")
	}
	return nil
}

// parseChallenge splits `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value := ""
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				value, rest = after[1:], ""
			} else {
				value, rest = after[1:end+1], after[end+2:]
			}
		} else {
			value, rest, _ = strings.Cut(after, ",")
			rest = "," + rest
		}
		params[key] = value
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}
	return scheme, params
}
