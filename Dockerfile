FROM golang:1.24.1 AS build

# Set the working directory
WORKDIR /app

# Copy go.mod and go.sum, then download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the Go application with CGO disabled and statically linked
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main ./cmd/main.go

# Use a minimal base image for production
FROM alpine:latest

# Install docker-cli for docker cp and docker exec commands
RUN apk add --no-cache docker-cli

# Set working directory
WORKDIR /root/

# Copy the built binary from the build stage
COPY --from=build /app/main .

# Expose the application port
EXPOSE 8080

# Run the application
CMD ["./main"]
