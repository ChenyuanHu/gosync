#!/bin/bash

# 显示颜色设置
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

# 显示编译开始信息
echo -e "${YELLOW}开始编译GoSync多机文件同步工具...${NC}"

# 检查Go环境
if ! command -v go &> /dev/null; then
    echo -e "${RED}错误: Go未安装，请安装Go环境后再试${NC}"
    exit 1
fi

# 显示Go版本
GO_VERSION=$(go version)
echo -e "${GREEN}当前Go版本: ${GO_VERSION}${NC}"

# 获取当前操作系统和架构
CURRENT_OS=$(go env GOOS)
CURRENT_ARCH=$(go env GOARCH)
echo -e "${GREEN}当前操作系统和架构: ${CURRENT_OS}/${CURRENT_ARCH}${NC}"

# 默认编译为当前系统架构
TARGET_OS=${1:-linux}
TARGET_ARCH=${2:-amd64}

# 编译输出文件名
OUTPUT_NAME="gosync"
if [ "$TARGET_OS" != "$CURRENT_OS" ] || [ "$TARGET_ARCH" != "$CURRENT_ARCH" ]; then
    OUTPUT_NAME="gosync-${TARGET_OS}-${TARGET_ARCH}"
fi

echo -e "${YELLOW}编译目标: ${TARGET_OS}/${TARGET_ARCH}，输出文件: ${OUTPUT_NAME}${NC}"

# 设置Go环境变量，进行交叉编译
export GOOS=$TARGET_OS
export GOARCH=$TARGET_ARCH
export CGO_ENABLED=0

# 编译
echo -e "${YELLOW}正在编译...${NC}"
go build -o $OUTPUT_NAME -ldflags="-s -w" main.go

# 检查编译结果
if [ $? -eq 0 ]; then
    echo -e "${GREEN}编译成功!${NC}"
    echo -e "${GREEN}可执行文件: $(pwd)/${OUTPUT_NAME}${NC}"
    
    # 添加执行权限
    chmod +x $OUTPUT_NAME
    
    # 显示文件大小
    FILE_SIZE=$(du -h $OUTPUT_NAME | cut -f1)
    echo -e "${GREEN}文件大小: ${FILE_SIZE}${NC}"
    
    # 显示文件类型
    echo -e "${GREEN}文件类型: $(file $OUTPUT_NAME)${NC}"
else
    echo -e "${RED}编译失败!${NC}"
    exit 1
fi

# 显示使用说明
echo -e "${YELLOW}使用说明:${NC}"
echo -e "${GREEN}./gosync -dir=<要同步的目录> -peers=<对等节点IP列表,以逗号分隔> [-port=<端口号>]${NC}"

exit 0 