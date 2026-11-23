// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "PocketBaseMCPChat",
    platforms: [
        .macOS("26.0"),
        .iOS("26.0"),
    ],
    products: [
        .library(
            name: "PBMCPChat",
            targets: ["PBMCPChat"]
        ),
        .library(
            name: "PBMCPChatUI",
            targets: ["PBMCPChatUI"]
        ),
    ],
    dependencies: [
        .package(url: "https://github.com/gonzalezreal/swift-markdown-ui", exact: "2.4.1"),
    ],
    targets: [
        .target(
            name: "PBMCPChat",
            dependencies: []
        ),
        .target(
            name: "PBMCPChatUI",
            dependencies: [
                "PBMCPChat",
                .product(name: "MarkdownUI", package: "swift-markdown-ui"),
            ]
        ),
        .testTarget(
            name: "PBMCPChatTests",
            dependencies: ["PBMCPChat"],
            resources: [
                .copy("Fixtures"),
            ]
        ),
        .testTarget(
            name: "PBMCPChatUITests",
            dependencies: ["PBMCPChat", "PBMCPChatUI"]
        ),
    ]
)
