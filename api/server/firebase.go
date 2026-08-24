package main

import (
	"context"
	"fmt"
	"os"

	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
)

// FirebaseClients bundles the Firebase Admin SDK clients used by the BFF.
//   - Auth verifies ID tokens minted by the Flutter app (email-link sign-in).
//   - Firestore stores the users/{uid} profile, including the linked bemolClientId.
//
// Credentials come from Application Default Credentials: on Cloud Run this is the
// attached service account, locally it is `gcloud auth application-default login`.
type FirebaseClients struct {
	Auth      *auth.Client
	Firestore *firestore.Client
}

// projectID resolves the GCP project from the environment. On Cloud Run
// GOOGLE_CLOUD_PROJECT is injected automatically; FIREBASE_PROJECT_ID is an
// explicit override for local development.
func projectID() string {
	if p := os.Getenv("GOOGLE_CLOUD_PROJECT"); p != "" {
		return p
	}
	return os.Getenv("FIREBASE_PROJECT_ID")
}

// initFirebase initializes the Auth and Firestore clients. Returns an error if
// either client cannot be created so main() can decide whether to run degraded.
func initFirebase(ctx context.Context) (*FirebaseClients, error) {
	pid := projectID()

	conf := &firebase.Config{}
	if pid != "" {
		conf.ProjectID = pid
	}
	app, err := firebase.NewApp(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("firebase.NewApp: %w", err)
	}

	authClient, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("app.Auth: %w", err)
	}

	// firestore.DetectProjectID lets the client read the project from ADC/metadata
	// when no explicit project id is configured.
	fsProject := pid
	if fsProject == "" {
		fsProject = firestore.DetectProjectID
	}
	fsClient, err := firestore.NewClient(ctx, fsProject)
	if err != nil {
		return nil, fmt.Errorf("firestore.NewClient: %w", err)
	}

	return &FirebaseClients{Auth: authClient, Firestore: fsClient}, nil
}
