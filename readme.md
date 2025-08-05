# errcheckif linter

目标：

如果函数调用返回值包含`error`类型，那么这个`error`变量 `err` 必须在后续 `if` 语句中被检查，检查条件可以是：
* `err != nil`
* `err == nil`
* `errors.Is(err, ***)`
* `errors.As(err, ***)`

或者是通过 `return` 进行错误传递。

默认跳过测试文件（以`_test.go`结尾）。

## 添加到 golangci-lint

使用[官方](https://golangci-lint.run/plugins/module-plugins/#the-automatic-way)推荐的 `Module Plugin System` 方式


### 1. **定义构建的配置文件**

根目录下创建`.custom-gcl.yml`文件，填写以下内容

``` yaml
version: v2.3.0
plugins:
  - module: 'github.com/Ju-DeCo/errcheckif-linter' #指定仓库地址
    import: 'github.com/Ju-DeCo/errcheckif-linter/errcheckif' #指定包
    version: v0.1.9 #指定发布版本
```

### 2. **运行命令生成二进制文件**

``` 
golangci-lint custom -v
```
根路径下运行以上命令会生成一个可执行文件：

`custom-gcl.exe`

### 3. **在`.golangci.custom.yaml`中对自定义插件进行配置**

避免使用 `.golangci.yaml` 等官方配置文件名
``` yaml
version: "2"

linters:
  # 关闭其他所有插件
  default: none
  # 开启自定义插件
  enable:
    - errcheckif
  settings:
    custom:
      errcheckif:
        type: "module" # 设置为 module 类型
        description: "a custom linter"
```

### 4. **运行生成的二进制文件**

执行以下命令进行检测

```
# 清除缓存
golangci-lint cache clean

# custom-gcl为生成的二进制文件名
# .golangci.custom.yaml为自定义的yaml配置文件
./custom-gcl run --config .\.golangci.custom.yaml
```

## 测试举例
``` go
// 错误 1
_, err := mightFail()
fmt.Println(err) // 这里仅仅使用，没有检查 err

// 错误 2 (没有使用 err)
_, err = mightFail()

// 正确 1
_, err = mightFail()
if err != nil {
    panic(err)
}

// 正确 2
f, err := os.Open("non-existent-file.txt")
if errors.Is(err, os.ErrNotExist) {
    fmt.Println("file does not exist")
}
defer f.Close()

// 正确 3
_, err = mightFail()
if err == nil {
    // 这种检查虽然不常见，但语法上没错，我们暂时不处理
}

// 正确 4
_, err = mightFail()
if errors.As(err, &os.ErrNotExist) {
    fmt.Println("file does not exist")
}

func error_propagation() (string, error) {
    // 正确 5 错误传递
    fail, err := mightFail()
    return fail, err
}

// 正确6 裸返回
func test_naked_return() (err error) {
    err = errors.New("123")
    return
}
```


## 局限性

**控制流误报**
``` go
if 1 < 2 {
    _, err = mightFail()
} else {
    _, err = mightFail()
}
if err != nil {
}

```

**并发误报**
``` go
go func() {
    var terr error
    defer func() {
        if terr != nil {
        }
    }()
    terr = fail()
}()
```

可通过 `nolint:errcheckif` 跳过检测。