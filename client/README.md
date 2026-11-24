# otel-exporter-client

Typed client for the otel-exporter module's routes, generated from
`../openapi.json`. Run `make client` after changing that file and commit the
result; `make check` fails if they differ.

```ts
import { bind } from 'otel-exporter-client';

const routes = bind(host.client);
const status = await routes.readExportStatus({}, signal);
```

`src/generated/` is generated. `src/index.ts`, `tsconfig.json` and this README
are maintained by hand.
