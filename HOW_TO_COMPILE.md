# How to Compile

Follow these steps to build the application from source.

## Prerequisites

* [Go](https://golang.org/)

## Steps

1. Clone the repository and navigate to the project directory:
2. Move into the `source` directory where the source code and build files are located:
3. Build the executable using the following command:
```bash
go build -trimpath -ldflags "-s -w" -o goebook.exe
```

The compiled `goebook.exe` will be generated in the `source` folder, complete with the embedded icon and version information from the `.syso` file.

Note that the version number must be manually updated whenever a new release comes out to keep track of the version.
