"""CLI entry point for Legion state collection."""

from __future__ import annotations

import argparse
import json
import sys
import uuid
from typing import Any

import anyio

from legion.state import decision, fetch


def parse_args() -> argparse.Namespace:
    """Parse command line arguments."""
    parser = argparse.ArgumentParser(
        description="Collect Legion state and suggest actions"
    )
    parser.add_argument(
        "--team-id",
        required=True,
        help="Linear team UUID",
    )
    parser.add_argument(
        "--short-id",
        required=True,
        help="Short project ID for tmux sessions",
    )
    parser.add_argument(
        "--session-dir",
        help="Claude session directory (for controller inspection)",
    )
    parser.add_argument(
        "--debug",
        action="store_true",
        help="Enable debug logging",
    )
    return parser.parse_args()


async def async_main() -> None:
    """Async CLI entrypoint."""
    args = parse_args()

    # Set up logging
    import logging

    level = logging.DEBUG if args.debug else logging.WARNING
    logging.basicConfig(
        level=level,
        format="%(levelname)s: %(name)s: %(message)s",
        stream=sys.stderr,
    )

    # Validate team-id is a valid UUID
    try:
        uuid.UUID(args.team_id)
    except ValueError:
        print("Error: --team-id must be a valid UUID", file=sys.stderr)
        sys.exit(1)

    # Read Linear issues from stdin
    linear_json = sys.stdin.read()
    try:
        linear_data = json.loads(linear_json)
        # Handle both raw list and wrapped format
        if isinstance(linear_data, list):
            issues: list[dict[str, Any]] = linear_data
        else:
            issues = linear_data.get("issues", [])
    except json.JSONDecodeError as e:
        print(f"Error parsing Linear JSON: {e}", file=sys.stderr)
        sys.exit(1)

    # Fetch all data
    issues_data = await fetch.fetch_all_issue_data(
        linear_issues=issues,
        short_id=args.short_id,
    )

    # Build and output state
    state = decision.build_collected_state(issues_data, args.team_id)
    print(json.dumps(state.to_dict(), indent=2))


def main() -> None:
    """CLI entrypoint: reads Linear JSON from stdin, outputs state JSON."""
    anyio.run(async_main)


if __name__ == "__main__":
    main()
