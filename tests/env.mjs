// 测试脚本的公共环境。
// BASE：被测 server 的地址（默认 4040）。
// LIMPET_HOME：server 的数据目录——必须与被测 server 启动时用的一致（socket 与录制都在这里）；
//              开发与测试把它指向 ~/.limpet-dev，与正式服务的数据（默认 ~/.limpet）分开，见 README。
import { homedir } from 'node:os'
import { join } from 'node:path'

export const BASE = process.env.BASE || 'http://127.0.0.1:4040'
export const WS = BASE.replace(/^http/, 'ws')
export const LIMPET_HOME = process.env.LIMPET_HOME || join(homedir(), '.limpet')
