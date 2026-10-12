# Switching root versions

`dot_firmware.py` moved to [firebreak](https://github.com/bboe/firebreak). It
takes the same flags, and its targets have new names:

| old target | firebreak target | the Dot ends with |
|---|---|---|
| `v1-bboe` (the default) | `amonet-biscuit-v1.1.0-bboe` (the default) | rooted Fire OS 5.5.5.4, TWRP 3.7.0_9-bboe2 |
| `v1` | `amonet-biscuit-v1.1.0` | the same, with TWRP 3.2.3 |
| `v2` | `amonet-biscuit-v2.0.0` | rooted Fire OS 6 8146 |
| `stock BUILD` | `stock BUILD` | stock Fire OS 6 at that build |

overdub runs only on Fire OS 5, so only the first two can host it.

## Getting the script

`uvx firebreak@latest` runs firebreak, or `python3 firebreak.pyz` from its
[releases](https://github.com/bboe/firebreak/releases).
[Run it](https://github.com/bboe/firebreak#run-it), in firebreak's README, has
the install commands for macOS, Linux and Windows.

## Targets

The table above maps the names.
[Targets](https://github.com/bboe/firebreak/blob/main/docs/rooting.md#targets),
in firebreak's docs/rooting.md, says how a run moves between them.

## Between v1-bboe and v1

`uvx firebreak@latest amonet-biscuit-v1.1.0` (was `v1`), or
`uvx firebreak@latest` (was `v1-bboe`).
[Targets](https://github.com/bboe/firebreak/blob/main/docs/rooting.md#targets)
says why it is one write.

## From v1-bboe or v1 to v2

`uvx firebreak@latest amonet-biscuit-v2.0.0` (was `v2`).
[amonet-biscuit-v2.0.0](https://github.com/bboe/firebreak/blob/main/docs/rooting.md#amonet-biscuit-v200)
has the steps.

## From v2 back to v1-bboe or v1

`uvx firebreak@latest` (was `v1-bboe`), or
`uvx firebreak@latest amonet-biscuit-v1.1.0` (was `v1`).
[Writing amonet v1.1.0 from v2.0.0's TWRP](https://github.com/bboe/firebreak/blob/main/docs/rooting.md#writing-amonet-v110-from-v200s-twrp)
has the steps.

## To and from stock

`uvx firebreak@latest stock 8146` (was `stock 8146`).
[Back to stock: the stock target](https://github.com/bboe/firebreak/blob/main/docs/rooting.md#back-to-stock-the-stock-target)
has the steps.

## A Dot that shows no light: `--short`

`uvx firebreak@latest --short`, with any target.
[`--short`](https://github.com/bboe/firebreak#--short), in firebreak's README,
says where to short and when.
