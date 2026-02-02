"""Simple farewell messages."""


def goodbye(name: str | None = None) -> str:
    """Return a goodbye message.

    Args:
        name: Optional name to include in the greeting.

    Returns:
        A goodbye message, personalized if name is provided.
    """
    if name and name.strip():
        return f"Goodbye, {name}!"
    return "Goodbye!"


def farewell(name: str | None = None) -> str:
    """Return a farewell message.

    Args:
        name: Optional name to include in the greeting.

    Returns:
        A farewell message, personalized if name is provided.
    """
    if name and name.strip():
        return f"Farewell, {name}!"
    return "Farewell!"
