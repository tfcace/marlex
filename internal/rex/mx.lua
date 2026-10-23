-- marlex for Rex: blame anything on screen.
--
-- A key reads the focused terminal, finds the file names and file:line
-- references on it (compiler errors, test failures, stack traces, grep
-- output, diffs, an agent's transcript, a file open in an editor's status
-- line), and opens `mx why -i` in a block beside it for the one
-- nearest the cursor (in an editor, the line under its cursor, followed as
-- it moves). In that block, down digs into what the line replaced and up
-- comes back, left and right move between the files on screen when there
-- are several, s shows the commit, y copies its hash, and q closes it.
--
-- `mx rex install` puts this file next to init.lua and adds:
--
--   require("mx").setup{ key = "cmd+shift+b" }
--
-- setup takes key (false for no key; the action "mx_why" is always in
-- the command palette), bin (the mx binary), open ("split" for a block
-- to the right, the default, or "layer" for a floating pane), width (the
-- split's share of the space, 0.45), and bounds (the floating pane's
-- {x, y, w, h} as fractions of the window).

local M = {
  bin = "mx",
  open = "split",
  width = 0.45,
  bounds = { 0.06, 0.08, 0.88, 0.84 },
}

-- The block marlex opened last, so a second press replaces it instead of
-- stacking splits.
local last

local T = "com.superlogical.terminal"

-- The directory references on screen are relative to, the foreground
-- program's (an editor's or an agent's) or else the shell's, and the
-- foreground program's name, which tells marlex when it is an editor.
local function program_of(block)
  local p = rex.block.call(T, "process", { block_id = block })
  if p then
    local fg = p.foreground or p.child or {}
    -- Under a login shell the foreground is /usr/bin/login, with no cwd;
    -- marlex finds an editor under its pid itself.
    if fg.cwd then return fg.cwd, fg.name, fg.pid end
    if p.child and p.child.cwd then return p.child.cwd, fg.name, fg.pid end
    local d = rex.block.call(T, "pwd", { block_id = block })
    if d and d.pwd then
      return (d.pwd:gsub("^file://[^/]*", "")), fg.name, fg.pid
    end
    return nil, fg.name, fg.pid
  end
  local d = rex.block.call(T, "pwd", { block_id = block })
  if d and d.pwd then
    return (d.pwd:gsub("^file://[^/]*", ""))
  end
end

function M.why(ctx)
  local block = ctx.block_id
  if not block then
    return nil, "no terminal is focused"
  end
  local text, err = rex.block.call(T, "format", { block_id = block, format = "text" })
  if not text then
    return nil, err
  end
  -- The cursor is where the reader is looking: the prompt under the output
  -- that just printed, or an editor's line.
  local row, col = 0, 0
  local vt = rex.block.call(T, "format", { block_id = block, format = "vt", extra_cursor = true })
  if vt and vt.content then
    local r, c = vt.content:match(".*\27%[(%d+);(%d+)H")
    row, col = tonumber(r) or 0, tonumber(c) or 0
  end
  local size = rex.block.call(T, "size", { block_id = block }) or {}

  local path = os.tmpname()
  local f, ferr = io.open(path, "w")
  if not f then
    return nil, ferr
  end
  f:write(text.content)
  f:close()

  local cwd, program, pid = program_of(block)
  local t = rex.block.call(T, "title", { block_id = block })
  local argv = {
    M.bin, "why", "-i", "--screen", path, "--rm-screen",
    "--cursor", row .. "," .. col,
    "--cols", tostring(size.columns or 0),
    "--rows", tostring(size.rows or 0),
    "--program", program or "",
    "--pid", tostring(pid or 0),
    "--title", t and t.title or "",
    -- follow the block as it changes, which matters in an editor
    "--follow", block, "--session", ctx.session_id or "",
  }
  local layout = rex.layout.block{
    flavor = "com.superlogical.terminal.shell",
    options = { command = argv, shell = "none", cwd = cwd },
  }
  if last and last ~= block then
    rex.call("block.close", { session_id = ctx.session_id, block_id = last })
  end
  local res, oerr
  if M.open == "layer" then
    res, oerr = rex.session.new_layer{ bounds = M.bounds, focus = true, layout = layout }
  else
    res, oerr = rex.session.new_split{
      anchor_block_id = block,
      direction = "horizontal",
      side = "after",
      ratio = 1 - M.width,
      focus = true,
      layout = layout,
    }
  end
  if res and res.block_ids then
    last = res.block_ids[1]
  end
  return res, oerr
end

function M.setup(opts)
  opts = opts or {}
  M.bin = opts.bin or M.bin
  M.bounds = opts.bounds or M.bounds
  M.open = opts.open or M.open
  M.width = opts.width or M.width
  rex.action{
    name = "mx_why",
    title = "mx: why is this here?",
    description = "Blame the file or line nearest the cursor, in any program",
    category = "mx",
    keywords = "blame history git log",
    run = function(ctx) return M.why(ctx) end,
  }
  if opts.key ~= false then
    rex.bind(opts.key or "cmd+shift+b", function(ctx)
      local ok, err = M.why(ctx)
      if not ok then rex.log("warn", "mx: " .. tostring(err)) end
      return true
    end)
  end
  return M
end

return M
