"""PostgreSQL fixture access and Prometheus parsing shared by suites."""
from __future__ import annotations
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from ..environment.kind import KindEnvironment
import json

def literal(value):
    return "'" + value.replace("'", "''") + "'"


def sql(m: KindEnvironment, statement):
    return m.k('exec', '-i', 'postgres', '--', 'psql', '-X', '-q', '-A', '-t', '-U', 'cache', '-d', 'products',
               '-v', 'ON_ERROR_STOP=1', data=statement).strip()


def source(m: KindEnvironment, key):
    value = sql(m, "SELECT json_build_object('version',version,'deleted',deleted,'value',convert_from(value,'UTF8')) "
                'FROM simplecache_products WHERE key=' + literal(key))
    result = json.loads(value)
    if not result['deleted']:
        result['value'] = json.loads(result['value'])
    return result


def metric(text, name):
    return sum(float(line.rsplit(' ', 1)[1]) for line in text.splitlines()
               if line.startswith(name + ' ') or line.startswith(name + '{'))

