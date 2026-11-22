package chserver

import (
	"net/http"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/middleware"
	"github.com/openrport/openrport/server/pairing"
	"github.com/openrport/openrport/share/security"
)

func (al *APIListener) initPairing() {
	cfg := al.config.Pairing
	al.pairing = pairing.NewService(pairing.Options{
		PublicURL:      al.config.PairingPublicURL(),
		DownloadsURL:   cfg.DownloadsURL,
		CodeTTL:        cfg.CodeTTL,
		AllowCodeReuse: cfg.AllowCodeReuse,
	}, al.Logger)
}

// registerPairingRoutes serves the built-in pairing service, outside the /api/v1 prefix
// so the layout equals the standalone service (POST /, GET /update, GET /{code}) under /pairing.
// These routes must be registered before the doc_root file server catch-all.
func (al *APIListener) registerPairingRoutes(r *mux.Router) {
	if !al.config.Pairing.Enabled {
		return
	}
	if al.pairing == nil {
		al.initPairing()
	}

	// Creating a code hands out client credentials: same access as /clients-auth (administrators).
	var deposit http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		al.pairing.HandleDeposit(w, r, api.GetUser(r.Context(), al.Logger))
	})
	deposit = al.wrapAdminAccessMiddleware(deposit)
	if !al.insecureForTests {
		deposit = al.wrapWithAuthMiddleware(false)(deposit)
	}
	if al.bannedIPs != nil {
		deposit = security.RejectBannedIPs(al.bannedIPs)(deposit)
	}
	if al.config.API.MaxRequestBytes > 0 {
		deposit = middleware.MaxBytes(deposit, al.config.API.MaxRequestBytes)
	}

	r.Handle(pairing.PathPrefix, deposit).Methods(http.MethodPost)
	r.HandleFunc(pairing.PathPrefix+"/update", al.pairing.HandleUpdate).Methods(http.MethodGet)
	r.HandleFunc(pairing.PathPrefix+"/{"+pairing.CodeVar+":[0-9A-Za-z]{1,64}}", al.pairing.HandleInstaller).Methods(http.MethodGet)
}

// pairingURL is the pairing service URL reported to the UI: the built-in one when
// enabled, otherwise [server] pairing_url (an external service).
func (al *APIListener) pairingURL(r *http.Request) string {
	if al.config.Pairing.Enabled && al.pairing != nil {
		return al.pairing.URL(r)
	}
	return al.config.Server.PairingURL
}
