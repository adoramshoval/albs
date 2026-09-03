package preflight

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sidecarExt replaces the output archive's own extension, so
// python-offline.cnb yields python-offline.versions.json alongside it.
const sidecarExt = ".versions.json"

// SidecarPath derives the report path from the archive path.
func SidecarPath(outputPath string) string {
	return strings.TrimSuffix(outputPath, filepath.Ext(outputPath)) + sidecarExt
}

// Write emits the report.
//
// It is called on the failure path too, before the error is returned: the
// table is the diagnosis, it names which stacks the tag does support, and CI
// can archive it from a run that produced no .cnb at all.
func Write(path string, r Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding preflight report: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing preflight report: %w", err)
	}
	return nil
}

// Summary is the one-line form for the build log, so a wildcard-only or
// partial result is not missed by someone who never opens the JSON.
func (r Report) Summary() string {
	if r.Identity.Stack == "" {
		return fmt.Sprintf("recorded %d components for %s (no --stack given, nothing asserted)",
			len(r.Components), r.Identity.Target)
	}

	var fromSource []string
	for _, c := range r.Components {
		if c.CoveredOnlyBySource {
			fromSource = append(fromSource, c.ID)
		}
	}

	var b strings.Builder
	if r.Verdict.Covered {
		fmt.Fprintf(&b, "coverage ok on %s for %s (groups %v)",
			r.Identity.Stack, r.Identity.Target, r.Verdict.SatisfiedGroups)
	} else {
		fmt.Fprintf(&b, "no coverage on %s for %s", r.Identity.Stack, r.Identity.Target)
	}
	if len(fromSource) > 0 {
		fmt.Fprintf(&b, "; %s will be compiled from source at build time", strings.Join(fromSource, ", "))
	}
	if !r.Verdict.Complete {
		b.WriteString("; report is partial")
	}
	return b.String()
}
