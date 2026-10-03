package main

import (
	"context"
	"fmt"
	"io"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/peoplemigration"
)

// migratePeople is `envoy-dispatch migrate-people`: it moves every person the database
// databaseURL (DATABASE_URL) names by GitHub login to the email peopleMap (DISPATCH_PEOPLE_MAP)
// gives them (peoplemigration.Run), writing each field's count of logins before and after to out. A
// deployment runs it once, as a one-off task of the Dispatch service's own task definition, with
// the service scaled to 0 so no room holds a document it renames. Exit 0 when every person moved,
// 1 when it refused (a login the map lacks, which changes nothing) or left something to move;
// errOut says which.
func migratePeople(ctx context.Context, peopleMap, databaseURL string, out, errOut io.Writer) int {
	people, err := peoplemigration.ParseMap(peopleMap)
	if err != nil {
		fmt.Fprintf(errOut, "migrate-people: %v\n", err)
		return 1
	}
	database, ok := openMigrated(ctx, "migrate-people", databaseURL, errOut)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	documents := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	err = peoplemigration.Run(ctx, database, documents, people, out)
	if shutdownErr := documents.Shutdown(context.Background()); shutdownErr != nil {
		fmt.Fprintf(errOut, "migrate-people: shut down documents: %v\n", shutdownErr)
		if err == nil {
			return 1
		}
	}
	if err != nil {
		fmt.Fprintf(errOut, "migrate-people: %v\n", err)
		return 1
	}
	return 0
}
