import { snippetSegments } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { Fragment, type KeyboardEvent, type ReactNode, useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { SearchResult, SearchResultKind } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  backdrop50,
  badgeLow,
  borderDefault,
  card,
  inputClasses,
  searchHitBg,
  searchHitText,
  selectedCardBg,
  selectedCardBorder,
  textMutedOnSelectedCard,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";
import { groupResults, kindLabel, optionId, stepActive } from "./search-model";

const emptyResults: SearchResult[] = [];
function SearchResultIcon({ kind }: { kind: SearchResultKind }): ReactNode {
  const common = {
    "aria-hidden": true,
    className: "size-4 shrink-0",
    fill: "none",
    stroke: "currentColor",
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    strokeWidth: 1.75,
    viewBox: "0 0 24 24",
  };

  if (kind === "issue") {
    return (
      <svg {...common}>
        <title>Issue result</title>
        <circle cx="12" cy="12" r="8" />
        <path d="M12 8v4l2.5 2" />
      </svg>
    );
  }
  if (kind === "document") {
    return (
      <svg {...common}>
        <title>Document result</title>
        <path d="M7 3h7l3 3v15H7z" />
        <path d="M14 3v4h4M10 12h4M10 16h4" />
      </svg>
    );
  }
  if (kind === "comment") {
    return (
      <svg {...common}>
        <title>Comment result</title>
        <path d="M5 6.5h14v10H9l-4 3z" />
        <path d="M9 10h6M9 13h4" />
      </svg>
    );
  }
  if (kind === "ask") {
    return (
      <svg {...common}>
        <title>Ask result</title>
        <circle cx="12" cy="12" r="8" />
        <path d="M9.8 9.5a2.3 2.3 0 1 1 3.3 2.1c-.9.5-1.1.9-1.1 1.9M12 16h.01" />
      </svg>
    );
  }
  return (
    <svg {...common}>
      <title>Message result</title>
      <path d="M5 6.5h14v10H9l-4 3z" />
      <path d="M9 10h6M9 13h6" />
    </svg>
  );
}

function ResultOption({
  active,
  muted,
  onSelect,
  result,
}: {
  active: boolean;
  muted: boolean;
  onSelect: () => void;
  result: SearchResult;
}): ReactNode {
  const segments = snippetSegments(result.snippet);
  let segmentStart = 0;

  return (
    <div
      aria-selected={active}
      className={`relative isolate min-h-11 cursor-pointer border-b px-3 py-2 last:border-b-0 ${borderDefault} ${
        active ? selectedCardBorder : ""
      }`}
      id={optionId(result)}
      onClick={onSelect}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onSelect();
        }
      }}
      role="option"
      tabIndex={-1}
    >
      {active ? (
        <div
          aria-hidden="true"
          className={`pointer-events-none absolute inset-0 -z-10 ${selectedCardBg}`}
        />
      ) : null}
      <div
        className={`flex min-w-0 items-center gap-2 text-xs ${
          muted ? textMutedOnSelectedCard : textSecondaryOnSurface
        }`}
      >
        <SearchResultIcon kind={result.kind} />
        <span className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}>
          {kindLabel(result.kind)}
        </span>
        {result.artifact === undefined ? null : (
          <span className="truncate">{result.artifact.name}</span>
        )}
      </div>
      <p
        className={`mt-1 line-clamp-2 text-sm ${
          muted ? textMutedOnSelectedCard : textPrimaryOnSurface
        }`}
      >
        {segments.map((segment) => {
          const key = `${segmentStart}-${segment.mark}`;
          segmentStart += segment.text.length;
          return segment.mark ? (
            <mark className={`rounded px-0.5 ${searchHitBg} ${searchHitText}`} key={key}>
              {segment.text}
            </mark>
          ) : (
            <Fragment key={key}>{segment.text}</Fragment>
          );
        })}
      </p>
    </div>
  );
}

export function SearchPalette({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}): ReactNode {
  const [query, setQuery] = useState("");
  const [debouncedQuery, setDebouncedQuery] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const lastLocationKey = useRef<string | undefined>(undefined);
  const location = useLocation();
  const navigate = useNavigate();
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: inputRef, onClose, open });
  const searchText = query.trim();
  const queryEnabled = searchText.length >= 2;

  useEffect(() => {
    const timeout = window.setTimeout(() => setDebouncedQuery(searchText), 150);
    return () => window.clearTimeout(timeout);
  }, [searchText]);

  const search = useQuery({
    enabled: debouncedQuery.length >= 2,
    queryFn: () => api.search(debouncedQuery),
    queryKey: ["search", debouncedQuery],
  });
  const results = search.data?.results ?? emptyResults;
  const groups = groupResults(results);
  const visibleResults = groups.flatMap(({ results }) => results);

  useEffect(() => {
    if (search.dataUpdatedAt > 0) {
      setActiveIndex(0);
    }
  }, [search.dataUpdatedAt]);

  useEffect(() => {
    const changed =
      lastLocationKey.current !== undefined && lastLocationKey.current !== location.key;
    lastLocationKey.current = location.key;
    if (open && changed) {
      onClose();
    }
  }, [location.key, onClose, open]);

  if (!open) {
    return null;
  }

  const activeResult = visibleResults[activeIndex];
  const waitingForQuery = queryEnabled && debouncedQuery !== searchText;
  const navigateToResult = (result: SearchResult) => {
    navigate(result.href);
    onClose();
  };
  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown" && visibleResults.length > 0) {
      event.preventDefault();
      setActiveIndex((index) => stepActive(index, 1, visibleResults.length));
    } else if (event.key === "ArrowUp" && visibleResults.length > 0) {
      event.preventDefault();
      setActiveIndex((index) => stepActive(index, -1, visibleResults.length));
    } else if (event.key === "Enter" && activeResult !== undefined) {
      event.preventDefault();
      navigateToResult(activeResult);
    }
  };

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-x-0 top-12 z-50 flex justify-center xl:top-20 xl:px-4">
        <div
          aria-label="Search"
          aria-modal="true"
          className={`pointer-events-auto w-full overflow-hidden rounded-b-lg border shadow-2xl xl:max-w-2xl xl:rounded-lg ${card} ${borderDefault}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <div className={`border-b p-3 ${borderDefault}`}>
            <input
              aria-activedescendant={
                activeResult === undefined ? undefined : optionId(activeResult)
              }
              aria-controls="search-results"
              aria-expanded={results.length > 0}
              aria-label="Search"
              className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
              onChange={(event) => setQuery(event.target.value)}
              onKeyDown={handleKeyDown}
              placeholder="Search Dispatch"
              ref={inputRef}
              role="combobox"
              type="search"
              value={query}
            />
          </div>
          {!queryEnabled ? (
            <p className={`px-3 py-3 text-sm ${textMutedOnSurface}`}>Type at least 2 characters</p>
          ) : waitingForQuery || search.isPending ? (
            <p className={`px-3 py-3 text-sm ${textMutedOnSurface}`}>Searching…</p>
          ) : search.isError ? (
            <div className="p-3">
              <QueryError
                message="Search failed."
                onRetry={() => {
                  void search.refetch();
                }}
                retrying={search.isFetching}
              />
            </div>
          ) : results.length === 0 ? (
            <p className={`px-3 py-3 text-sm ${textMutedOnSurface}`}>
              No results for &quot;{searchText}&quot;
            </p>
          ) : (
            <div id="search-results" role="listbox">
              {groups.map((group) => (
                <Fragment key={group.issue.key}>
                  <div
                    aria-label={`${group.issue.key}: ${group.issue.title}`}
                    className={`flex min-w-0 items-center gap-2 border-b px-3 py-2 text-xs ${borderDefault} ${
                      group.issue.status === "done" ? textMutedOnSurface : ""
                    }`}
                    data-status={group.issue.status}
                    role="presentation"
                  >
                    <span className="font-medium">{group.issue.key}</span>
                    <span
                      className={`min-w-0 flex-1 truncate ${
                        group.issue.status === "done" ? textMutedOnSurface : textSecondaryOnSurface
                      }`}
                    >
                      {group.issue.title}
                    </span>
                    <span
                      className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}
                    >
                      {group.issue.status}
                    </span>
                  </div>
                  {group.results.map((result) => (
                    <ResultOption
                      active={result === activeResult}
                      muted={group.issue.status === "done"}
                      key={optionId(result)}
                      onSelect={() => navigateToResult(result)}
                      result={result}
                    />
                  ))}
                </Fragment>
              ))}
            </div>
          )}
        </div>
      </div>
    </>
  );
}
