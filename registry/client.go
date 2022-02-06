package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	dockerHubHost                      = "registry-1.docker.io"
	mediaTypeDockerSchema2ManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerSchema2Manifest     = "application/vnd.docker.distribution.manifest.v2+json"
)

// Repository represents a repository using Docker Registry API v2.
type Repository struct {
	client   *http.Client
	host     string
	repo     string
	user     string
	password string
	token    string
}

// New creates a client for a repository.
func New(image, user, password string) *Repository {
	c := &Repository{
		client:   &http.Client{},
		user:     user,
		password: password,
	}
	p := strings.SplitN(image, "/", 2)
	if strings.Contains(p[0], ".") && len(p) >= 2 {
		// Docker registry v2 API
		c.host = p[0]
		c.repo = p[1]
	} else {
		// DockerHub
		if !strings.Contains(image, "/") {
			image = "library/" + image
		}
		c.host = dockerHubHost
		c.repo = image
	}
	return c
}

func (c *Repository) login(endpoint, service, scope string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	u.RawQuery = strings.Join([]string{
		"service=" + url.QueryEscape(service),
		"scope=" + url.QueryEscape(scope),
	}, "&")
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if c.user != "" && c.password != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("login failed %s", resp.Status)
	}
	dec := json.NewDecoder(resp.Body)
	var body struct {
		Token string `json:"Token"`
	}
	if err := dec.Decode(&body); err != nil {
		return err
	}
	if body.Token == "" {
		return errors.New("response does not contains token")
	}
	c.token = body.Token
	return nil
}

func (c *Repository) fetchManifests(method, tag string) (*http.Response, error) {
	u := fmt.Sprintf("https://%s/v2/%s/manifests/%s", c.host, c.repo, tag)
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", strings.Join([]string{
		mediaTypeDockerSchema2ManifestList,
		ocispec.MediaTypeImageIndex,
		mediaTypeDockerSchema2Manifest,
		ocispec.MediaTypeImageManifest}, ", "))
	c.setAuthHeader(req)
	return c.client.Do(req)
}

func (c *Repository) getAvailability(tag string) (*http.Response, error) {
	resp, err := c.fetchManifests(http.MethodHead, tag)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, nil
}

func (c *Repository) getManifests(tag string) (mediaType string, _ io.ReadCloser, _ error) {
	resp, err := c.fetchManifests(http.MethodGet, tag)
	if err != nil {
		return "", nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return "", nil, errors.New(resp.Status)
	}
	mediaType = parseContentType(resp.Header.Get("Content-Type"))
	return mediaType, resp.Body, nil
}

func (c *Repository) getImageConfig(digest string) (io.ReadCloser, error) {
	u := fmt.Sprintf("https://%s/v2/%s/blobs/%s", c.host, c.repo, digest)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.container.image.v1+json",
		ocispec.MediaTypeImageConfig,
	}, ", "))
	c.setAuthHeader(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, errors.New(resp.Status)
	}
	return resp.Body, err
}

func (c *Repository) setAuthHeader(req *http.Request) {
	if c.user == "AWS" && c.password != "" {
		// ECR
		req.Header.Set("Authorization", "Basic "+c.password)
	} else if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func parseContentType(contentType string) (mediaType string) {
	mediaType = contentType
	if i := strings.IndexByte(contentType, ';'); i != -1 {
		mediaType = contentType[0:i]
	}
	return
}

func match(want, got string) bool {
	// if want is empty, skip verify
	return want == "" || want == got
}

var ErrDeprecatedManifest = errors.New("deprecated manifest")

// HasPlatformImage returns an image tag for arch/os exists or not in the repository.
func (c *Repository) HasPlatformImage(tag, arch, os string) error {
	mediaType, rc, err := c.getManifests(tag)
	if err != nil {
		return err
	}
	defer rc.Close()
	dec := json.NewDecoder(rc)
	switch mediaType {
	case
		ocispec.MediaTypeImageIndex,
		mediaTypeDockerSchema2ManifestList:
		var manifestList ocispec.Index
		if err := dec.Decode(&manifestList); err != nil {
			return fmt.Errorf("manifest list decode error: %w", err)
		}
		// https://github.com/opencontainers/image-spec/blob/main/image-index.md#image-index-property-descriptions
		for _, desc := range manifestList.Manifests {
			p := desc.Platform
			if p == nil {
				// regard as non platform-specific image
				return nil
			}
			if match(arch, p.Architecture) && match(os, p.OS) {
				return nil
			}
		}
	case
		mediaTypeDockerSchema2Manifest,
		ocispec.MediaTypeImageManifest:
		var manifest ocispec.Manifest
		if err := dec.Decode(&manifest); err != nil {
			return fmt.Errorf("manifest decode error: %w", err)
		}
		if p := manifest.Config.Platform; p != nil {
			if match(arch, p.OS) && match(os, p.Architecture) {
				return nil
			}
		}

		// fallback to image config
		// https://github.com/opencontainers/image-spec/blob/main/config.md#properties
		rc, err := c.getImageConfig(manifest.Config.Digest.String())
		if err != nil {
			return err
		}
		defer rc.Close()
		var image ocispec.Image
		if err := json.NewDecoder(rc).Decode(&image); err != nil {
			return fmt.Errorf("image config decode error: %w", err)
		}
		if match(arch, image.Architecture) && match(os, image.OS) {
			return nil
		}
	case
		// https://docs.docker.com/registry/spec/deprecated-schema-v1/
		"application/vnd.docker.distribution.manifest.v1+prettyjws",
		"application/vnd.docker.distribution.manifest.v1+json":
		return ErrDeprecatedManifest
	default:
		return fmt.Errorf("unknown MediaType %s", mediaType)
	}

	return fmt.Errorf("no image for %s/%s", arch, os)
}

// HasImage returns an image tag exists or not in the repository.
func (c *Repository) HasImage(tag string) (bool, error) {
	tries := 2
	for tries > 0 {
		tries--
		resp, err := c.getAvailability(tag)
		if err != nil {
			return false, err
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			h := resp.Header.Get("Www-Authenticate")
			if strings.HasPrefix(h, "Bearer ") {
				auth := strings.SplitN(h, " ", 2)[1]
				if err := c.login(parseAuthHeader(auth)); err != nil {
					return false, err
				}
			}
		case http.StatusOK:
			return true, nil
		default:
			return false, errors.New(resp.Status)
		}
	}
	return false, errors.New("aborted")
}

var (
	partRegexp = regexp.MustCompile(`[a-zA-Z0-9_]+="[^"]*"`)
)

func parseAuthHeader(bearer string) (endpoint, service, scope string) {
	parsed := make(map[string]string, 3)
	for _, part := range partRegexp.FindAllString(bearer, -1) {
		kv := strings.SplitN(part, "=", 2)
		parsed[kv[0]] = kv[1][1 : len(kv[1])-1]
	}
	return parsed["realm"], parsed["service"], parsed["scope"]
}
