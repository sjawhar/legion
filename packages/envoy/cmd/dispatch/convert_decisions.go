package main

import (
 "context"
 "flag"
 "fmt"
 "io"
 "strings"

 "github.com/sjawhar/envoy/internal/dispatch/docs"
 "github.com/sjawhar/envoy/internal/dispatch/events"
 "github.com/sjawhar/envoy/internal/dispatch/store"
)

func convertDecisions(ctx context.Context,args []string,databaseURL string,out io.Writer) int {
 flags:=flag.NewFlagSet("convert-decisions",flag.ContinueOnError)
 flags.SetOutput(out)
 apply:=flags.Bool("apply",false,"move legacy records into spec nodes and remove their source documents (default only reports)")
 if err:=flags.Parse(args);err!=nil{return 2}
 if flags.NArg()!=0{fmt.Fprintln(out,"convert-decisions: no positional arguments; use --apply only after reviewing the dry run");return 2}
 if strings.TrimSpace(databaseURL)==""{fmt.Fprintln(out,"convert-decisions: DATABASE_URL is required");return 1}
 // Unlike openMigrated, a dry run must not apply even a schema migration.
 database,err:=store.Open(ctx,databaseURL)
 if err!=nil{fmt.Fprintf(out,"convert-decisions: %v\n",err);return 1}
 defer database.Pool.Close()
 service:=docs.New(docs.Deps{Store:database,Events:events.NewBroker()})
 defer service.Shutdown(context.Background())
 if _,err:=service.ConvertDecisionDocuments(ctx,*apply,out);err!=nil{fmt.Fprintf(out,"convert-decisions: %v\n",err);return 1}
 return 0
}
