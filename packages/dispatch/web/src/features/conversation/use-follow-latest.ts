import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

const BOTTOM_SLACK_PX = 64;

function pinnedToBottom(): boolean {
  return (
    window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - BOTTOM_SLACK_PX
  );
}

export function useFollowLatest({
  enabled,
  itemSeqs,
  ownSendCount,
}: {
  enabled: boolean;
  itemSeqs: readonly number[];
  ownSendCount: number;
}): {
  atBottom: boolean;
  jumpToLatest: () => void;
  newItemCount: number;
  pinnedToBottom: () => boolean;
} {
  const [atBottom, setAtBottom] = useState(true);
  const [newItemCount, setNewItemCount] = useState(0);
  const latestItemSeq = itemSeqs.at(-1);
  const atBottomRef = useRef(true);
  const previousItemSeq = useRef<number | undefined>(undefined);
  const previousSends = useRef(ownSendCount);
  const visibleThroughSeq = useRef<number | undefined>(undefined);

  const scrollToBottom = useCallback(() => {
    window.scrollTo({ top: document.documentElement.scrollHeight });
    visibleThroughSeq.current = latestItemSeq;
    setNewItemCount(0);
  }, [latestItemSeq]);

  useEffect(() => {
    const onScroll = () => {
      const nextAtBottom = pinnedToBottom();
      const wasAtBottom = atBottomRef.current;
      atBottomRef.current = nextAtBottom;
      setAtBottom(nextAtBottom);
      if (wasAtBottom !== nextAtBottom) {
        visibleThroughSeq.current = latestItemSeq;
        setNewItemCount(0);
      }
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    onScroll();
    return () => window.removeEventListener("scroll", onScroll);
  }, [latestItemSeq]);

  useLayoutEffect(() => {
    if (!enabled || latestItemSeq === undefined) {
      return;
    }
    const first = previousItemSeq.current === undefined;
    const ownSend = ownSendCount !== previousSends.current;
    previousSends.current = ownSendCount;
    const changed = previousItemSeq.current !== latestItemSeq;
    previousItemSeq.current = latestItemSeq;
    if (!changed) {
      return;
    }
    if (first || ownSend || atBottomRef.current) {
      scrollToBottom();
      return;
    }
    const visibleThrough = visibleThroughSeq.current ?? latestItemSeq;
    setNewItemCount(itemSeqs.filter((seq) => seq > visibleThrough).length);
  }, [enabled, itemSeqs, latestItemSeq, ownSendCount, scrollToBottom]);

  return {
    atBottom,
    jumpToLatest: scrollToBottom,
    newItemCount,
    pinnedToBottom: () => atBottom || pinnedToBottom(),
  };
}
