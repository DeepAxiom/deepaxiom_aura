# Builds the example app for docker-compose. The SDK is taken from the repo
# rather than from a registry, because it is not published yet — see
# GUIDE.md#references for what that means for anyone depending on it.
#
# It is taken as SOURCE, and that is the part worth stating: package.json
# points main at ./dist/index.js, but sdk/node/dist is generated and
# .gitignore keeps it out of the tree — unlike ui/dist, which is committed
# because the kernel embeds it. So `npm install --omit=dev` in a single stage
# cannot produce it: the compiler is a devDependency and --omit=dev is exactly
# what drops it. The app then starts, fails to resolve @deepaxiom/aura, and the
# capability assertion downstream reports the symptom rather than this cause.
#
# Hence two stages: the SDK is compiled with its dev dependencies present, then
# pruned, and the app installs a package that already has its dist/.
FROM node:22-alpine AS sdk
WORKDIR /sdk/node
COPY sdk/node/package.json sdk/node/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY sdk/node/ ./
RUN npm run build && npm prune --omit=dev

FROM node:22-alpine
WORKDIR /app
COPY --from=sdk /sdk/node /sdk/node
COPY examples/expose-app/package.json ./
RUN npm install --omit=dev --no-audit --no-fund
COPY examples/expose-app/app.mjs ./
USER node
CMD ["node", "app.mjs"]
