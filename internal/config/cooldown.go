package config

// dependabotConfig is the subset of Dependabot's schema this tool reads.
type dependabotConfig struct {
	Updates []struct {
		PackageEcosystem string    `yaml:"package-ecosystem"`
		Cooldown         *cooldown `yaml:"cooldown"`
	} `yaml:"updates"`
}

// cooldown mirrors Dependabot's cooldown block. See
// dependabot-core common/lib/dependabot/package/release_cooldown_options.rb.
type cooldown struct {
	DefaultDays     int      `yaml:"default-days"`
	SemverMajorDays int      `yaml:"semver-major-days"`
	SemverMinorDays int      `yaml:"semver-minor-days"`
	SemverPatchDays int      `yaml:"semver-patch-days"`
	Include         []string `yaml:"include"`
	Exclude         []string `yaml:"exclude"`
}

// actionsCooldown returns the cooldown block from the first github-actions
// update entry that has one.
func (c dependabotConfig) actionsCooldown() (cooldown, bool) {
	for _, u := range c.Updates {
		if u.PackageEcosystem == "github-actions" && u.Cooldown != nil {
			return *u.Cooldown, true
		}
	}
	return cooldown{}, false
}

// unsupportedWarnings names cooldown keys that are set but not yet honored.
func (c cooldown) unsupportedWarnings() []string {
	var w []string
	if c.SemverMajorDays > 0 || c.SemverMinorDays > 0 || c.SemverPatchDays > 0 {
		w = append(w, "Dependabot cooldown semver-major/minor/patch-days are not supported and were ignored")
	}
	if len(c.Include) > 0 || len(c.Exclude) > 0 {
		w = append(w, "Dependabot cooldown include/exclude filters are not supported and were ignored")
	}
	return w
}
