"""Pure logic: no I/O, no environment reads, no SDK imports.

Keeping decisions here is what lets tests/test_logic.py run under plain
python3, with no runtime and no daemon. Add vendor clients and mapping code in
this directory as the integration grows; main.py stays the wiring.
"""


def next_count(current):
    """Return the value after current, treating a missing value as 0."""
    return (current or 0) + 1
