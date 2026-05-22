package scheduler

import (
	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// composeProject aliases the parsed compose project we get from
// compose.LoadBytes. lookupImage scans Services for the matching name.
type composeProject = composetypes.Project

func lookupImage(p *composeProject, serviceName string) string {
	if p == nil {
		return ""
	}
	for _, s := range p.Services {
		if s.Name == serviceName {
			return s.Image
		}
	}
	return ""
}
