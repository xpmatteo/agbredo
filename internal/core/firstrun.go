package core

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// A deployment needs no registration by hand: the first valid token request
// from a new repository + workflow file creates it. The GitHub App
// installation is then the only gate on which repositories can send telemetry.

// MintTokenForWorkflow is MintToken for a runner that does not name its
// deployment. The deployment is the live one with the identity's repository
// and workflow file; owner and repository ignore case. If there is none, the
// first valid request creates it: enabled, with the default threshold, and
// with the installation of the GitHub App on the repository, which the App
// itself is asked for. Nothing is created when the App is not installed
// (ErrNotInstalled) or when no token is given, because the deployment, the run
// and the token are stored in one transaction.
func (s *Service) MintTokenForWorkflow(ctx context.Context, agentRun string, i Identity) (Grant, error) {
	if err := checkTokenRequest(agentRun, i); err != nil {
		return Grant{}, err
	}
	owner, repository, ok := strings.Cut(i.Repository, "/")
	if !ok {
		return Grant{}, ErrUnauthorized
	}
	existing, err := findDeployment(ctx, s.DB, owner, repository, i.Workflow)
	if err == nil {
		return s.mint(ctx, agentRun, i, func(pgx.Tx) (string, error) { return existing.ID, nil })
	}
	if !errors.Is(err, ErrNotFound) {
		return Grant{}, err
	}
	installation, err := s.Backend.Installation(ctx, owner, repository)
	if err != nil {
		return Grant{}, err
	}
	// Owner and repository come from the OIDC token, in GitHub's own spelling.
	fresh := Deployment{ID: ID(), Name: defaultName(owner, repository, i.Workflow), Owner: owner, Repository: repository, Workflow: i.Workflow,
		InstallationID: installation, ThresholdMicros: s.DefaultThresholdMicros, Enabled: true, Revision: 1}
	if err = ValidateDeployment(fresh); err != nil {
		return Grant{}, invalid("this workflow cannot become a deployment: " + err.Error())
	}
	return s.mint(ctx, agentRun, i, func(tx pgx.Tx) (string, error) { return insertDeployment(ctx, tx, fresh) })
}

// findDeployment finds the live deployment of a repository + workflow file.
// Owner and repository ignore case, as in the unique index
// (migrations/006_unique_deployment.sql).
func findDeployment(ctx context.Context, q querier, owner, repository, workflow string) (Deployment, error) {
	return readJSON[Deployment](ctx, q, "SELECT document FROM agents WHERE lower(document->>'owner')=lower($1) AND lower(document->>'repository')=lower($2) AND document->>'workflow'=$3 AND NOT deleted", owner, repository, workflow)
}

// insertDeployment stores a deployment created by its first run and returns
// its ID. When another request is creating the same deployment, the unique
// index makes this wait for it; if that one is kept, its ID is returned.
func insertDeployment(ctx context.Context, tx pgx.Tx, a Deployment) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO agents(id,document) VALUES($1,$2)
		ON CONFLICT (lower(document->>'owner'), lower(document->>'repository'), (document->>'workflow')) WHERE NOT deleted DO NOTHING
		RETURNING id`, a.ID, mustJSON(a)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, e := findDeployment(ctx, tx, a.Owner, a.Repository, a.Workflow)
		return existing.ID, e
	}
	return id, err
}

// defaultName names a deployment created by its first run, e.g.
// "acme/agent · triage.yml", cut to the 120 bytes a name may hold.
func defaultName(owner, repository, workflow string) string {
	name := owner + "/" + repository + " · " + workflow
	for len(name) > 120 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}
