import { useLocation } from "react-router-dom";

import { isLegacyLogPath, parseIssuePath } from "../refs/routes";

export function useIssueRoute() {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);

  return { isLegacyLogPath: isLegacyLogPath(pathname), route, search };
}
