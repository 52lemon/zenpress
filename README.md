Toropress
=========
#基于ToroPress开发的社会化平台
##正在构建中

*[building](http://this.com)


Toropress是SudoChina.com的开发代号,
是一个简单和强大的Golang CMS系统网站，作小许改动后可以应用为Blog、企业站、小说站、图站等多种类型网站..

第一个版本基于[Torgo](https://github.com/insionng/torgo)([Beego](https://github.com/astaxie/beego)山寨版)与[Qbs](https://github.com/coocood/qbs)开发.

第二版于2013年10月1日更新,由于beego最新版本几乎已经把[Torgo](https://github.com/insionng/torgo)的所有修改优化吸收完全了,从而决定了Toropress源码再次回归[Beego](https://github.com/astaxie/beego),并采用了[Xorm](https://github.com/lunny/xorm),目前已经将[Qbs](https://github.com/coocood/qbs)代码完全迁移到[Xorm](https://github.com/lunny/xorm),这将获得更好的使用体验,感谢那些捐助本项目的用户,如需定制请联系我.



# 通过捐款支持Toropress项目
如果你喜欢这个项目的话， 可以通过捐款的方式， 支持作者继续更新本项目或者做出其他更多好玩好用的开源应用： 比如为本项目修补漏洞、添加更多有趣的功能， 或者发行有更多更棒特性的下一版等等。

捐款地址： [https://me.alipay.com/insion](https://me.alipay.com/insion)



##第二版安装请先更新安装beego和xorm

    go get -u github.com/astaxie/beego
    go get -u github.com/lunny/xorm

##第一版安装请先更新torgo

    go get -u github.com/insionng/torgo

##安装
	先安装sqlite3驱动，譬如64位的win7：
	SQLITE3驱动编译环境是TDM版MINGW64(http://tdm-gcc.tdragon.net/)，
    安装好后请把TDM版MINGW64的bin目录路径加入到你的win7环境变量path里面。

	
	go get -u github.com/mattn/go-sqlite3

    下载toropress源码后解压并cd切换目录到 toropress目录下，然后执行go build app.go，编译好后，运行./app即可。
    默认用户:root,默认密码:rootpass




## 交流
欢迎大家加入QQ专用交流群:231956113/作者QQ：547092001

技术分享：[http://www.sudochina.com](http://www.sudochina.com)


## 授权许可
除特别声明外，本项目代码遵循[BSD 3-Clause License](<http://opensource.org/licenses/BSD-3-Clause/>)（3项条款的BSD许可协议）。

