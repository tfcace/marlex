-- Streams a terminal's screen to `mx why -i --follow`: one JSON line
-- each time the text, the cursor, the title or the foreground program
-- changes, so the
-- view can follow an editor as its cursor moves. Rex has no event for a
-- changed screen, so it looks four times a second. It stops when the block
-- closes; marlex stops it when the view closes.
local T = "com.superlogical.terminal"
local block = rex.args.block
local last
while true do
  local text = rex.block.call(T, "format", { block_id = block, format = "text" })
  if not text then
    rex.stop(0)
  end
  local row, col = 0, 0
  local vt = rex.block.call(T, "format", { block_id = block, format = "vt", extra_cursor = true })
  if vt and vt.content then
    local r, c = vt.content:match(".*\27%[(%d+);(%d+)H")
    row, col = tonumber(r) or 0, tonumber(c) or 0
  end
  local size = rex.block.call(T, "size", { block_id = block }) or {}
  local p = rex.block.call(T, "process", { block_id = block })
  local t = rex.block.call(T, "title", { block_id = block })
  local msg = rex.json.encode{
    text = text.content,
    row = row,
    col = col,
    rows = size.rows or 0,
    cols = size.columns or 0,
    program = p and p.foreground and p.foreground.name or "",
    pid = p and p.foreground and p.foreground.pid or 0,
    title = t and t.title or "",
  }
  if msg ~= last then
    last = msg
    io.write(msg, "\n")
    io.flush()
  end
  rex.sleep(0.25)
end
