# python/tests/conftest.py
# Pytest konfigurace a sdílené fixtures
import sys
from pathlib import Path

# Zajisti, že root projektu je v Python path
root = Path(__file__).parent.parent.parent
if str(root) not in sys.path:
    sys.path.insert(0, str(root))
