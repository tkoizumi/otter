"""The entrypoint.

The runtime runs python3 main.py with this directory as the working directory
and the SDK's otter package already on PYTHONPATH. State, logs and the trigger
arrive on the context.
"""

from otter import Context

from logic import next_count

ctx = Context.from_environment()

count = next_count(ctx.state.get("count"))

ctx.log.info("ran", count=count, trigger=ctx.trigger.type)

ctx.state.set("count", count)
