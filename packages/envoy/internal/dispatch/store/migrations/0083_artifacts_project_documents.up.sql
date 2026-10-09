-- 0083_artifacts_project_documents.up.sql
-- A project's unlinked documents: the artifacts with neither an issue nor an agent conversation.
-- Settlement of a project document looks up the other documents of its project whose asks a copied
-- ask block may have come from, and the copies of an ask it retracts, on every settlement of a
-- document holding a block no ask of its own indexes (docs.ownerDocuments). Without this index each
-- such settlement scans artifacts; the issue branch already reads artifacts_issue_key. The index
-- predicate is the query's own, so the planner can use it.
create index artifacts_project_documents
  on artifacts (project_key)
  where issue_key is null and session_id is null;
