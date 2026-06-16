package ghapi

import (
	"context"
	"fmt"
	"net/http"
)

// OrgHostedRunnerNames fetches the names (runs-on labels) of all
// GitHub-hosted runners configured for the given org. Returns nil on
// permission errors (403) so callers can fall back to the static list.
func (c *Client) OrgHostedRunnerNames(ctx context.Context, org string) ([]string, error) {
	var names []string
	page := 1
	for {
		path := fmt.Sprintf("orgs/%s/actions/hosted-runners?per_page=100&page=%d", org, page)
		var resp struct {
			TotalCount int `json:"total_count"`
			Runners    []struct {
				Name string `json:"name"`
			} `json:"runners"`
		}
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			if IsPermissionDenied(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("listing org hosted runners: %w", err)
		}
		for _, r := range resp.Runners {
			names = append(names, r.Name)
		}
		if len(names) >= resp.TotalCount {
			break
		}
		page++
	}
	return names, nil
}
