package opsconsole

import (
	"fmt"

	"github.com/projanvil/woca/internal/ee/authcore"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/skillsvc"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/settings"
	"github.com/rs/zerolog"
)

// Assembly is the console authority fully built over the shared settings
// database: the console service plus the stores an entry point may mount
// on its own surfaces (woca-scheduler's /gw machine channel; the woca-ops
// facade needs only the console).
type Assembly struct {
	Console *Service          // the console API (mount via Handler)
	GW      *entrygw.Store    // gateway registry (machine channel, audit trail)
	Keys    *tenant.Store     // tenant credentials (native-client channel)
	Auth    *authcore.Service // login/session authority (/gw/auth)
}

// Assemble builds the console authority's store chain over the shared
// settings database — gateway registry, tenant credentials, auth core,
// and the skill-asset store — wired into one console Service with the
// skill API already mounted (AttachSkills). Every entry point that owns
// the console builds it here (woca-ops's direct assembly, woca-scheduler's
// management listener), so a console-owned store is added exactly once,
// never per entry point. The caller keeps its listener/mux/SPA posture
// and decides how a failure terminates (Fatal or degrade): Assemble only
// reports the error, with the failing link named.
func Assemble(st *settings.Store, log zerolog.Logger) (*Assembly, error) {
	gw, err := entrygw.NewStore(st.DB())
	if err != nil {
		return nil, fmt.Errorf("gateway registry store: %w", err)
	}
	keys, err := tenant.NewStore(st.DB())
	if err != nil {
		return nil, fmt.Errorf("tenant store: %w", err)
	}
	auth, err := authcore.New(keys, st.DB(), log)
	if err != nil {
		return nil, fmt.Errorf("core auth service: %w", err)
	}
	skills, err := skillsvc.NewStore(st.DB())
	if err != nil {
		return nil, fmt.Errorf("skill asset store: %w", err)
	}
	console := New(st, gw, auth, keys, log)
	console.AttachSkills(skills)
	return &Assembly{Console: console, GW: gw, Keys: keys, Auth: auth}, nil
}
