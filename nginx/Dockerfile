FROM node:alpine as builder

WORKDIR /app

COPY ../client/ .

RUN corepack pnpm install && corepack pnpm build

FROM nginx:alpine

COPY --from=builder /app/build /www