"""Runtime-free tests.

No daemon, no SDK, no network: python3 -m unittest discover -s tests
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from logic import next_count  # noqa: E402


class NextCount(unittest.TestCase):
    def test_starts_at_one_when_nothing_is_stored(self):
        self.assertEqual(next_count(None), 1)

    def test_increments_what_is_stored(self):
        self.assertEqual(next_count(41), 42)


if __name__ == "__main__":
    unittest.main()
