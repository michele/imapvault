package engine

import (
	"context"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/mail"
)

// doctorFeatures are the capabilities backup cares about and what each is for.
var doctorFeatures = []struct{ Name, Use string }{
	{"CONDSTORE", "fast flag-change detection"},
	{"QRESYNC", "fast expunge detection"},
	{"OBJECTID", "skip downloading a message already stored (not used yet)"},
	{"SPECIAL-USE", "folder roles such as \\Trash and \\Sent"},
	{"UIDPLUS", "needed by restore"},
}

// Feature is one capability a server does or does not offer.
type Feature struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Use       string `json:"use"`
}

// DoctorFolder is a folder as the server lists it.
type DoctorFolder struct {
	Name       string `json:"name"`
	SpecialUse string `json:"special_use,omitempty"`
	Messages   uint32 `json:"messages"`
	Selected   bool   `json:"selected"` // would be backed up under the current folders.include/exclude
	NoSelect   bool   `json:"noselect,omitempty"`
}

// DoctorReport is what `doctor` prints.
type DoctorReport struct {
	Capabilities []string       `json:"capabilities"`
	Features     []Feature      `json:"features"`
	Folders      []DoctorFolder `json:"folders"`
	Selected     int            `json:"selected_messages"` // messages in selected folders
}

// Doctor connects (through src) and reports capabilities and folders. It needs no
// repository and changes nothing.
func Doctor(ctx context.Context, cfg *config.Config, src mail.Source) (*DoctorReport, error) {
	caps, err := src.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	folders, err := src.ListFolders(ctx)
	if err != nil {
		return nil, err
	}
	rep := &DoctorReport{Capabilities: caps}
	for _, f := range doctorFeatures {
		rep.Features = append(rep.Features, Feature{Name: f.Name, Supported: caps.Has(f.Name), Use: f.Use})
	}
	for _, f := range folders {
		sel := !f.NoSelect && cfg.Folders.Selected(f.Name)
		rep.Folders = append(rep.Folders, DoctorFolder{Name: f.Name, SpecialUse: f.SpecialUse, Messages: f.Messages, Selected: sel, NoSelect: f.NoSelect})
		if sel {
			rep.Selected += int(f.Messages)
		}
	}
	return rep, nil
}
