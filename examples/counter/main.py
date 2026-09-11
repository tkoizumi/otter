from otter import Context

ctx = Context.from_environment()

count = ctx.state.get("count") or 0
count += 1

ctx.log.info("Counter executed", count=count)

ctx.state.set("count", count)
