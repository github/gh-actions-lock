package dockerimage

import (
	"fmt"
	"strings"
)

// Warning names mutable container images that an action commit cannot pin.
func Warning(image, via string) string {
	if !strings.HasPrefix(image, "docker://") || strings.Contains(image, "@") {
		return ""
	}
	if via != "" {
		image += " (pulled by " + via + ")"
	}
	return fmt.Sprintf("%s is not covered by the actions lockfile; container image tags are mutable", image)
}
