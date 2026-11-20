package report

import (
	"net/http"

	"github.com/sclevine/agouti"
)

// Compile-time proof that a live, logged-in Agouti page satisfies the Page
// interface this package drives. Using the real type here keeps the bridge
// contract honest as the Agouti API evolves.
var _ Page = (*agouti.Page)(nil)

// Compile-time proof that an Agouti page can seed the shared HTTP session.
var _ interface {
	GetCookies() ([]*http.Cookie, error)
} = (*agouti.Page)(nil)
