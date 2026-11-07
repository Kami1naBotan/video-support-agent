# syntax=docker/dockerfile:1

FROM node:24-bookworm-slim AS build

WORKDIR /src/frontend

COPY frontend/package.json frontend/package-lock.json ./

RUN npm ci

COPY frontend/ ./

RUN npm run build

FROM nginx:stable-alpine AS runtime

COPY deploy/nginx/default.conf /etc/nginx/conf.d/default.conf

COPY --from=build /src/frontend/dist/ /usr/share/nginx/html/

EXPOSE 80

STOPSIGNAL SIGQUIT

CMD ["nginx", "-g", "daemon off;"]