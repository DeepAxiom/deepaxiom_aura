# Builds the example app for docker-compose. The SDK is taken from the repo
# rather than from a registry, because it is not published yet — see
# GUIDE.md#references for what that means for anyone depending on it.
FROM node:22-alpine
WORKDIR /app
COPY sdk/node /sdk/node
COPY examples/expose-app/package.json ./
RUN npm install --omit=dev --no-audit --no-fund
COPY examples/expose-app/app.mjs ./
USER node
CMD ["node", "app.mjs"]
