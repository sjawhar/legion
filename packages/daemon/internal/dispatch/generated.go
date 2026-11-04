package dispatch

// BlockUnits is the longest one block of a body Dispatch accepts, in UTF-16 code units as
// Dispatch counts them. Generated from DISPATCH_BLOCK_UNITS in packages/contracts, the value the
// server enforces and GET /api/v1/limits publishes, so the daemon's refusal at a call and the
// server's refusal of the post cannot drift apart.
const BlockUnits = 32000

// ItemBlocks is the most blocks one body Dispatch accepts may hold. Generated from
// DISPATCH_ITEM_BLOCKS in packages/contracts, the value the server enforces and
// GET /api/v1/limits publishes.
const ItemBlocks = 32768
