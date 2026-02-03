"""Pytest configuration for Legion tests."""

import pytest


def pytest_addoption(parser: pytest.Parser) -> None:
    """Add custom command line options."""
    parser.addoption(
        "--run-tmux",
        action="store_true",
        default=False,
        help="Run tmux integration tests",
    )


def pytest_configure(config: pytest.Config) -> None:
    """Register custom markers."""
    config.addinivalue_line(
        "markers",
        "tmux_integration: mark test as tmux integration test (deselected by default)",
    )


def pytest_collection_modifyitems(
    config: pytest.Config, items: list[pytest.Item]
) -> None:
    """Skip tmux_integration tests unless --run-tmux is passed."""
    if config.getoption("--run-tmux"):
        return

    skip_tmux = pytest.mark.skip(reason="need --run-tmux option to run")
    for item in items:
        if "tmux_integration" in item.keywords:
            item.add_marker(skip_tmux)
