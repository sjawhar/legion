---
title: "GitHub GraphQL Organization/User Fallback Pattern"
category: github-api
tags: ["graphql", "access-control", "fallback", "testing"]
date: "2026-03-11"
status: "active"
---

# GitHub GraphQL Organization/User Fallback Pattern

## Context

GitHub's GraphQL API bifurcates project data based on owner type: `organization { projectV2 }` for org-owned projects and `user { projectV2 }` for user-owned projects. When an authenticated user is not a member of an organization, queries using `organization(login: $owner)` fail with access errors, requiring fallback to `user(login: $owner)`.

## Core Pattern: Sticky Fallback with Error Classification

```typescript
let useUserQuery = false;

while (hasNextPage) {
  if (!useUserQuery) {
    try {
      // Try organization query first
      response = await executeGraphQLQuery(ORG_QUERY, ...);
      
      // Check for access-related GraphQL errors
      if (response.errors?.some(error => 
        error.message.includes("Could not resolve to an Organization") ||
        error.message.includes("not a member")
      )) {
        useUserQuery = true;
        response = await executeGraphQLQuery(USER_QUERY, ...);
      }
    } catch (error) {
      // Handle CLI exceptions for access errors
      if (isAccessError(error)) {
        useUserQuery = true;
        response = await executeGraphQLQuery(USER_QUERY, ...);
      } else {
        throw error; // Re-throw non-access errors
      }
    }
  } else {
    // Once fallback triggered, use user query for all subsequent pages
    response = await executeGraphQLQuery(USER_QUERY, ...);
  }
}
```

## Key Technical Decisions

### 1. Sticky State Optimization
- **Decision**: Once `useUserQuery` is set to true, skip organization queries for all subsequent pagination calls
- **Rationale**: If page 1 requires user query, page 2 will too. Avoids doubling API calls for user-owned projects
- **Impact**: Critical for pagination performance through large user projects

### 2. Dual-Channel Error Handling
- **Decision**: Handle both GraphQL `errors` array and CLI exceptions separately
- **Rationale**: GitHub CLI (`gh`) surfaces the same logical access error through different channels depending on response format
- **Pattern**: Always implement error classification in both contexts

### 3. Error Classification Strategy
```typescript
const isAccessError = (error: unknown): boolean => {
  const message = error instanceof Error ? error.message : String(error);
  return message.includes("Forbidden") ||
         message.includes("not a member") ||
         message.includes("Could not resolve to an Organization");
};
```

## Reusable Patterns

### 1. GitHub Schema Bifurcation Handler
This pattern applies to any GitHub GraphQL scenario where data lives under different roots based on owner type:
- Repository queries (`organization { repositories }` vs `user { repositories }`)
- Team queries
- Sponsorship queries

### 2. CommandRunner Injection for Testing
```typescript
export async function fetchData(
  runner: CommandRunner = defaultRunner
): Promise<Data> {
  // Implementation uses runner for all CLI calls
}
```
Enables comprehensive testing without mocking `child_process`.

### 3. Query String Templates
Extract shared GraphQL structure into templates to avoid duplication:
```typescript
const buildProjectQuery = (rootType: "organization" | "user") => `
  query($owner: String!, $number: Int!, $first: Int!, $after: String) {
    ${rootType}(login: $owner) {
      projectV2(number: $number) {
        // ... shared fields
      }
    }
  }
`;
```

## Testing Strategies

### 1. Call Sequence Verification
```typescript
const mockRunner: CommandRunner = async (cmd: string[]) => {
  callCount++;
  const queryArg = cmd.find(arg => arg.startsWith("query="));
  
  if (callCount === 1) {
    expect(queryArg).toContain("organization(login:");
    // Return access error
  } else {
    expect(queryArg).toContain("user(login:");
    // Return success
  }
};
```

### 2. Pagination-Through-Fallback Testing
Verify that after fallback on page 1, page 2 goes directly to user query (no re-probing):
```typescript
// callCount should be 3 (org fail, user success, user page 2)
// NOT 4 (org fail, user success, org fail again, user page 2)
```

### 3. Error Path Coverage
Test both GraphQL errors and CLI exceptions for each error type:
- GraphQL errors array: `{ errors: [{ message: "Could not resolve..." }] }`
- CLI exceptions: `throw new Error("Forbidden")`

## Edge Cases and Gotchas

### 1. Query Duplication Maintenance Risk
**Issue**: Having separate `ORG_QUERY` and `USER_QUERY` constants creates risk of divergence
**Mitigation**: Use template functions to generate queries from shared structure
**Current Risk**: Medium - adding fields to one query but not the other causes silent data loss

### 2. Error String Brittleness
**Issue**: Access detection relies on exact substring matching of error messages
**Risk Areas**:
- `"Forbidden"` is overly broad - could match rate limit errors
- GitHub could change error messages without notice
**Mitigation**: Consider more specific error patterns or GraphQL error codes when available

### 3. Mid-Pagination Failure Scenario
**Issue**: What happens if org query succeeds for page 1 but fails on page 2?
**Current Behavior**: Attempts user query with org-generated cursor - will fail
**Recommendation**: Add test coverage for this scenario

### 4. Labels Field Type Complexity
**Issue**: GraphQL response structure for labels field requires runtime type gymnastics
**Pattern**: `node.labels as unknown as ExpectedType` reveals schema/type mismatches
**Solution**: Ensure TypeScript interfaces match actual GraphQL response shapes

## Architecture Insights

### 1. GitHub's Split-Brain Design
Projects V2 data architecture forces bifurcation handling on all consumers. No polymorphic `owner` root exists - each owner type has its own schema branch.

### 2. CLI vs Direct GraphQL Tradeoffs
Using `gh` CLI simplifies auth but creates string-based error surfaces instead of typed HTTP responses. This dual-channel error handling pattern will recur in any `gh`-based integration.

### 3. Access Model Asymmetry
- Org members can query both org and user projects
- Non-members can only query user projects
- Always try org first (more common case), then fall back to user

## Future Applications

This pattern is immediately applicable to:
- Repository listing and management
- Team membership queries
- Organization insights and metrics
- Any GitHub API integration where owner type determines access scope

The error classification and sticky fallback patterns are broadly applicable beyond GitHub to any API with hierarchical access models.