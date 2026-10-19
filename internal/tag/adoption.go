package tag

import (
	"context"
	"fmt"
	"sort"
	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
)

type candidateDate struct {
	iso   string
	found bool
}

// CheckAdoption rejects candidates younger than the effective cooldown.
// It never selects a different commit: manifests belong to the resolved SHA.
func (tl *Lister) CheckAdoption(ctx context.Context, owner, repo, ref, sha string, now time.Time) error {
	days := tl.cooldown.CooldownDays(owner, repo)
	if days <= 0 {
		return nil
	}
	date, err := tl.cachedDate(ctx, owner, repo, sha, true)
	if err != nil {
		return fmt.Errorf("cannot establish cooldown age: %w", err)
	}
	newest, err := parseCandidateDate(date.iso, "commit committer date")
	if err != nil {
		return err
	}
	source := "commit committer date"
	if err := checkCandidateAge(newest, source, days, now); err != nil {
		return err
	}

	tags, err := tl.ListTags(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("cannot establish cooldown release age: %w", err)
	}
	// ListTags permits optional enrichment failures; adoption must not.
	if _, err := tl.fetchReleaseTags(ctx, owner, repo); err != nil {
		return fmt.Errorf("cannot establish cooldown release age: %w", err)
	}
	names := make(map[string]bool)
	if !parserlock.IsFullSha(ref) {
		names[ref] = true
	}
	for _, tag := range tags {
		if tag.MatchesSHA(sha) {
			names[tag.Name] = true
		}
	}
	orderedNames := make([]string, 0, len(names))
	for name := range names {
		orderedNames = append(orderedNames, name)
	}
	sort.Strings(orderedNames)
	for _, name := range orderedNames {
		release, err := tl.releaseDate(ctx, owner, repo, name)
		if err != nil {
			return fmt.Errorf("cannot establish cooldown release age: %w", err)
		}
		if !release.found {
			continue
		}
		published, err := parseCandidateDate(release.iso, "release "+name+" published_at")
		if err != nil {
			return err
		}
		if published.After(newest) {
			newest, source = published, "release "+name+" published_at"
		}
	}
	return checkCandidateAge(newest, source, days, now)
}

func parseCandidateDate(iso, source string) (time.Time, error) {
	date, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot establish cooldown age: missing or invalid %s %q", source, iso)
	}
	return date, nil
}

func checkCandidateAge(date time.Time, source string, days int, now time.Time) error {
	// Compare whole days without converting arbitrary configuration values to
	// time.Duration, which would overflow for very large cooldowns.
	if now.Before(date) || now.Sub(date).Hours()/24 < float64(days) {
		return fmt.Errorf("release cooldown requires %d days: %s is %s; wait for the candidate to age before adopting it",
			days, source, date.UTC().Format(time.RFC3339))
	}
	return nil
}

func (tl *Lister) releaseDate(ctx context.Context, owner, repo, name string) (candidateDate, error) {
	if dates, ok := tl.releaseDates.Get(ghapi.ForRepo(owner, repo)); ok {
		if iso, found := dates[name]; found {
			return candidateDate{iso: iso, found: true}, nil
		}
	}
	return tl.cachedDate(ctx, owner, repo, name, false)
}

func (tl *Lister) cachedDate(ctx context.Context, owner, repo, ref string, commit bool) (candidateDate, error) {
	kind := "release:"
	if commit {
		kind = "commit:"
	}
	key := ghapi.ForNWORef(owner, repo, kind+ref)
	if cached, ok := tl.dateCache.Get(key); ok {
		return cached, nil
	}
	result, err, _ := tl.dateSF.Do(key.String(), func() (any, error) {
		if cached, ok := tl.dateCache.Get(key); ok {
			return cached, nil
		}
		var date candidateDate
		if commit {
			iso, err := tl.client.CommitDate(ctx, owner, repo, ref)
			if err != nil {
				return nil, err
			}
			date = candidateDate{iso: iso, found: true}
		} else {
			release, err := tl.client.ReleaseByTag(ctx, owner, repo, ref)
			if err != nil {
				return nil, err
			}
			if release != nil {
				date = candidateDate{iso: release.PublishedAt, found: true}
			}
		}
		tl.dateCache.Put(key, date)
		return date, nil
	})
	if err != nil {
		return candidateDate{}, err
	}
	return result.(candidateDate), nil
}
