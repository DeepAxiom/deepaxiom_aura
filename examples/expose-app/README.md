# expose-app — adding AI to a backend you already have

Six lines of integration. Two functions that already existed become skills; one
of them is a write, and the kernel gates and seals it without this app writing a
line of gate-handling code.

```bash
node app.mjs   # AURA_WS_URL and AURA_TOKEN from the environment
```

Verified end to end: the gate fires on a graph that declares no gate, the real
function runs only after approval, and the effect appears in `aura audit` as
`decision=gate outcome=delivered`.

Mint the token with a scope rather than handing this process the operator's:

```bash
aura token issue --capability motor.api.shop.refund_order --label "shop backend"
```
