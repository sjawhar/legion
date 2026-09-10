import { useCallback, useEffect, useRef, useState } from "react";

import type { Issue } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";

export type IssueUpdateInput = Partial<Pick<Issue, "route" | "status" | "title">>;

export interface IssueUpdateMutation {
  mutate: (input: IssueUpdateInput) => void;
  variables: IssueUpdateInput | undefined;
}

export interface IssueDrafts {
  discardTitle: () => void;
  discardRoute: () => void;
  onIssueSettled: (error: unknown) => void;
  onIssueSuccess: (next: Issue, input: IssueUpdateInput) => void;
  requestRouteSubmit: () => boolean;
  requestStatusSubmit: (status: string) => boolean;
  requestTitleSubmit: () => boolean;
  retry: () => void;
  route: string;
  routeDirty: boolean;
  routeIsValid: boolean;
  title: string;
  titleDirty: boolean;
  titleError: string | null;
  writeRoute: (next: string) => void;
  writeTitle: (next: string) => void;
}

const routePattern = /^(role:[a-z0-9-]+|session:[0-9a-f-]{16,})$/;

export function useIssueDrafts(issue: Issue, updateIssue: IssueUpdateMutation): IssueDrafts {
  const [title, setTitle] = useState(issue.title);
  const [titleDirty, setTitleDirty] = useState(false);
  const [titleError, setTitleError] = useState<string | null>(null);
  const [titlePendingSubmit, setTitlePendingSubmit] = useState<string | null>(null);
  const [route, setRoute] = useState(issue.route ?? "");
  const [routeDirty, setRouteDirty] = useState(false);
  const [routePendingSubmit, setRoutePendingSubmit] = useState<string | null>(null);
  const titleRef = useRef(title);
  const routeRef = useRef(route);
  const titleDirtyRef = useRef(false);
  const routeDirtyRef = useRef(false);
  const titlePendingRef = useRef<string | null>(null);
  const routePendingRef = useRef<string | null>(null);
  const updateIssueGuard = useSubmitGuard();

  const setPendingTitle = (value: string | null) => {
    titlePendingRef.current = value;
    setTitlePendingSubmit(value);
  };
  const setPendingRoute = (value: string | null) => {
    routePendingRef.current = value;
    setRoutePendingSubmit(value);
  };
  const setTitleDraftDirty = (dirty: boolean) => {
    titleDirtyRef.current = dirty;
    setTitleDirty(dirty);
  };
  const setRouteDraftDirty = (dirty: boolean) => {
    routeDirtyRef.current = dirty;
    setRouteDirty(dirty);
  };
  const writeTitle = useCallback(
    (next: string) => {
      titleRef.current = next;
      setTitle(next);
      const dirty = next !== issue.title;
      titleDirtyRef.current = dirty;
      setTitleDirty(dirty);
      setTitleError(null);
    },
    [issue.title]
  );
  const writeRoute = useCallback(
    (next: string) => {
      routeRef.current = next;
      setRoute(next);
      const dirty = next !== (issue.route ?? "");
      routeDirtyRef.current = dirty;
      setRouteDirty(dirty);
    },
    [issue.route]
  );
  const applyServerTitle = useCallback((next: string) => {
    titleRef.current = next;
    setTitle(next);
  }, []);
  const applyServerRoute = useCallback((next: string) => {
    routeRef.current = next;
    setRoute(next);
  }, []);

  function drainPendingSubmits(): void {
    const pendingTitle = titlePendingRef.current;
    if (pendingTitle !== null) {
      updateIssueGuard.guard(() => updateIssue.mutate({ title: pendingTitle }));
      return;
    }
    const pendingRoute = routePendingRef.current;
    if (pendingRoute !== null) {
      updateIssueGuard.guard(() => updateIssue.mutate({ route: pendingRoute }));
    }
  }

  function requestTitleSubmit(): boolean {
    const submitted = titleRef.current.trim();
    if (issue.closed_at !== null || submitted === "") {
      setPendingTitle(null);
      setTitleError("Title is required.");
      return false;
    }
    setTitleError(null);
    if (submitted === issue.title) {
      applyServerTitle(submitted);
      setTitleDraftDirty(false);
      setPendingTitle(null);
      return false;
    }
    setPendingTitle(submitted);
    drainPendingSubmits();
    return true;
  }

  function requestRouteSubmit(): boolean {
    const submitted = routeRef.current;
    if (issue.closed_at !== null || (submitted !== "" && !routePattern.test(submitted))) {
      setPendingRoute(null);
      return false;
    }
    if (submitted === (issue.route ?? "")) {
      setRouteDraftDirty(false);
      setPendingRoute(null);
      return false;
    }
    setPendingRoute(submitted);
    drainPendingSubmits();
    return true;
  }

  const onIssueSuccess = (next: Issue, input: IssueUpdateInput) => {
    if (input.title !== undefined && titlePendingRef.current === input.title) {
      setPendingTitle(null);
    }
    if (input.route !== undefined && routePendingRef.current === input.route) {
      setPendingRoute(null);
    }
    if (titleRef.current.trim() === next.title) {
      applyServerTitle(next.title);
    }
    setTitleDraftDirty(titleRef.current.trim() !== next.title);
    setRouteDraftDirty(routeRef.current !== (next.route ?? ""));
  };
  const onIssueSettled = (error: unknown) => {
    updateIssueGuard.release();
    if (error === null) {
      drainPendingSubmits();
    }
  };
  const retry = () => {
    const { variables } = updateIssue;
    if (variables?.title !== undefined) {
      requestTitleSubmit();
      drainPendingSubmits();
      return;
    }
    if (variables?.route !== undefined) {
      requestRouteSubmit();
      drainPendingSubmits();
      return;
    }
    updateIssueGuard.retryLast(updateIssue);
  };
  const discardTitle = () => {
    applyServerTitle(issue.title);
    setTitleDraftDirty(false);
    setTitleError(null);
    setPendingTitle(null);
  };
  const discardRoute = () => {
    applyServerRoute(issue.route ?? "");
    setRouteDraftDirty(false);
    setPendingRoute(null);
  };
  const requestStatusSubmit = (status: string): boolean =>
    updateIssueGuard.guard(() => updateIssue.mutate({ status }));

  useEffect(() => {
    if (!titleDirty && !titleDirtyRef.current && titlePendingSubmit === null) {
      applyServerTitle(issue.title);
    }
  }, [applyServerTitle, issue.title, titleDirty, titlePendingSubmit]);
  useEffect(() => {
    if (!routeDirty && !routeDirtyRef.current && routePendingSubmit === null) {
      applyServerRoute(issue.route ?? "");
    }
  }, [applyServerRoute, issue.route, routeDirty, routePendingSubmit]);

  return {
    discardTitle,
    discardRoute,
    onIssueSettled,
    onIssueSuccess,
    requestRouteSubmit,
    requestTitleSubmit,
    retry,
    route,
    routeDirty,
    routeIsValid: route === "" || routePattern.test(route),
    title,
    requestStatusSubmit,
    titleDirty,
    titleError,
    writeRoute,
    writeTitle,
  };
}
