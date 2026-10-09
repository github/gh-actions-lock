package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
)

var proximaHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.ghe\.com$`)

// IsProxima reports whether hostname is a canonical tenant hostname.
func IsProxima(hostname string) bool { return proximaHost.MatchString(hostname) }

// PinHost binds a repository to its recorded host before any resolution starts.
// A repository cannot span hosts within a run: caches and ref selection are
// repository-scoped, and guessing would mix identities.
func (c *Client) PinHost(owner, repo, hostname string) error {
	if hostname == "" {
		hostname = c.Hostname
	}
	if hostname != c.Hostname && !(c.local != nil && hostname == "github.com") {
		return fmt.Errorf("%s/%s is pinned to %s, not the selected host %s", owner, repo, hostname, c.Hostname)
	}
	if c.local == nil {
		return nil
	}
	key := ForRepo(owner, repo)
	if prev, ok := c.pinned[key]; ok && prev != hostname {
		return fmt.Errorf("%s has conflicting lockfile hosts %s and %s", key, prev, hostname)
	}
	c.pinned[key] = hostname
	return nil
}

// ForRepo selects a host once per repository. Only a repository-level 404
// permits public fallback; missing refs, denied access and transport errors do not.
func (c *Client) ForRepo(ctx context.Context, owner, repo string) (*Client, error) {
	if c.local == nil {
		return c, nil
	}
	key := ForRepo(owner, repo)
	if selected, ok := c.routes.Get(key); ok {
		return selected, nil
	}
	v, err, _ := c.routeSF.Do(key.String(), func() (any, error) {
		if selected, ok := c.routes.Get(key); ok {
			return selected, nil
		}
		selected := c.local
		host := c.pinned[key]
		if host != "github.com" {
			_, err := c.local.repoMetadata(ctx, owner, repo)
			if err != nil {
				code, _ := StatusCode(err)
				if host != "" || code != http.StatusNotFound {
					return nil, err
				}
				selected = c.public
			}
		} else {
			selected = c.public
		}
		if selected == c.public {
			// ponytail: anonymous dotcom rate limit; use host-bound credentials
			// if public fallback volume exceeds that limit.
			meta, err := c.public.repoMetadata(ctx, owner, repo)
			if err != nil {
				return nil, err
			}
			if meta.Visibility != "public" {
				return nil, fmt.Errorf("github.com/%s is not a public fallback repository", key)
			}
		}
		c.routes.Put(key, selected)
		return selected, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Client), nil
}
