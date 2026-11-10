"""Explicit PyInstaller entry with private center-injected local bindings.

Build two onedir products with a fixed --runner-* name. No EXE is ever treated
as a Python interpreter, and --component-info imports no inference packages.
"""
import argparse
from contextlib import contextmanager, ExitStack
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile


def component_id():
    name = Path(sys.executable if getattr(sys, "frozen", False) else sys.argv[0]).stem
    if name == "demucs-runner":
        return name
    if name == "seedvc-runner":
        return name
    # Source-mode tests must opt into a fixed identifier, never a module path.
    return None


# Only the official complete bags; no signature or format guessing.
DEMUCS_GROUPS = {
    'htdemucs': ['955717e8'],
    'htdemucs_ft': ['f7e0c4bc', 'd12395a8', '92cfc3b6', '04573f0d'],
}


@contextmanager
def replaced(obj, name, value):
    # Preserve classmethod descriptors and inherited attributes as well as values.
    own = name in vars(obj)
    original = vars(obj).get(name)
    setattr(obj, name, value)
    try:
        yield
    finally:
        if own:
            setattr(obj, name, original)
        else:
            delattr(obj, name)


def denied(*_args, **_kwargs):
    raise RuntimeError('组件离线运行：禁止下载模型、默认缓存回退或首启编译')


@contextmanager
def offline():
    import socket
    keys = ('HF_HUB_OFFLINE', 'TRANSFORMERS_OFFLINE', 'HF_DATASETS_OFFLINE')
    previous = {key: os.environ.get(key) for key in keys}
    try:
        for key in keys:
            os.environ[key] = '1'
        with ExitStack() as stack:
            for obj, name in ((socket.socket, 'connect'), (socket.socket, 'connect_ex'),
                              (socket.socket, 'sendto'), (socket, 'create_connection'),
                              (socket, 'getaddrinfo'), (socket, 'gethostbyname'),
                              (socket, 'gethostbyname_ex')):
                stack.enter_context(replaced(obj, name, denied))
            yield
    finally:
        for key, value in previous.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


@contextmanager
def no_download_or_compile():
    import torch
    import torch.utils.cpp_extension as extensions
    import huggingface_hub
    with ExitStack() as stack:
        for obj, name in ((torch.hub, 'load'), (torch.hub, 'download_url_to_file'),
                          (torch.hub, 'load_state_dict_from_url'), (torch, 'compile'),
                          (extensions, 'load'), (extensions, 'load_inline'),
                          (huggingface_hub, 'hf_hub_download'),
                          (huggingface_hub, 'snapshot_download')):
            stack.enter_context(replaced(obj, name, denied))
        yield


def local_file(value):
    path = Path(value)
    if not path.is_absolute() or not path.is_file() or path.stat().st_size == 0:
        raise ValueError('绑定要求非空本地绝对文件路径: ' + str(path))
    return path.resolve()


def local_directory(value):
    path = Path(value)
    if not path.is_absolute() or not path.is_dir():
        raise ValueError('绑定要求本地绝对目录: ' + str(path))
    return path.resolve()


def wav_info(path):
    import wave
    with wave.open(str(path), 'rb') as audio:
        channels, rate, frames = audio.getnchannels(), audio.getframerate(), audio.getnframes()
        if channels not in (1, 2) or rate <= 0 or frames <= 0:
            raise ValueError('WAV 音频参数无效')
        remaining = frames
        while remaining:
            count = min(remaining, 65536)
            if len(audio.readframes(count)) != count * channels * audio.getsampwidth():
                raise ValueError('WAV 数据截断')
            remaining -= count
        return channels, rate, frames / rate


def verify_wav(path, source, channels, rate):
    actual_channels, actual_rate, duration = wav_info(path)
    source_duration = wav_info(source)[2]
    if (actual_channels != channels or actual_rate != rate or
            abs(duration - source_duration) > 0.1):
        raise ValueError('WAV 声道、采样率或时长与组件契约不符')


def demucs_binding(model, repo, bag_path, weights, weight_format):
    import yaml
    repo = local_directory(repo)
    bag_path = local_file(bag_path)
    component = (Path(sys.executable).resolve().parent if getattr(sys, 'frozen', False)
                 else Path(__file__).resolve().parent)
    fixed_bag = component / 'resources/demucs' / (model + '.yaml')
    if bag_path != fixed_bag or fixed_bag.resolve() != fixed_bag:
        raise ValueError('Demucs bag 必须是冻结组件固定资源路径的 YAML')
    bag = yaml.safe_load(bag_path.read_text(encoding='utf-8'))
    expected_weights = None if model == 'htdemucs' else [
        [1., 0., 0., 0.], [0., 1., 0., 0.],
        [0., 0., 1., 0.], [0., 0., 0., 1.]]
    if (not isinstance(bag, dict) or set(bag) - {'models', 'weights', 'segment'} or
            bag.get('models') != DEMUCS_GROUPS[model] or bag.get('weights') != expected_weights):
        raise ValueError('Demucs bag 不是支持的完整官方模型组')
    selected = {}
    suffix = '.th' if weight_format == 'th' else '.safetensors'
    for value in weights:
        path = local_file(value)
        sig = path.stem.rsplit('-', 1)[0] if weight_format == 'th' else path.stem
        if path.parent != repo or path.suffix != suffix or sig in selected:
            raise ValueError('Demucs 权重格式、repo 或重复签名不兼容')
        selected[sig] = path
    if set(selected) != set(DEMUCS_GROUPS[model]):
        raise ValueError('Demucs 必须显式绑定完整且同组的权重')
    if weight_format == 'th' and set(repo.glob('*.th')) != set(selected.values()):
        raise ValueError('Demucs repo 含未选择的 TH 权重')
    if weight_format == 'hf-safetensors' and any(repo.glob('*.th')):
        raise ValueError('HF repo 不得混用 TH 权重')
    return repo, bag, selected


@contextmanager
def demucs_local_loader(model, repo, bag, selected, weight_format):
    from demucs import api
    from demucs.apply import BagOfModels
    if weight_format == 'th':
        # LocalRepo retains native checksum validation and TH deserialization.
        from demucs.repo import LocalRepo
        local_repo = LocalRepo(repo)
        if local_repo.list_model() != selected:
            raise ValueError('Demucs repo 与显式选择的 TH 权重不符')
        decode = lambda sig: local_repo.get_model(sig)
    else:
        # Use the actual HF decoder, never torch.load or hub/cache resolution.
        try:
            from demucs.hf import load_safetensors_model
        except ImportError as exc:
            raise RuntimeError('所交付 Demucs 缺少 HF safetensors 格式加载器') from exc
        decode = lambda sig: load_safetensors_model(selected[sig])
    bound_repo = repo

    def load(name, repo=None):
        if name != model or repo is None or Path(repo).resolve() != bound_repo:
            raise ValueError('禁止 Demucs 默认或跨绑定加载')
        models = [decode(sig) for sig in bag['models']]
        return BagOfModels(models, bag.get('weights'), bag.get('segment')).eval()

    with replaced(api, 'get_model', load):
        yield


def demucs(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument('--model', choices=tuple(DEMUCS_GROUPS), required=True)
    ap.add_argument('--repo', required=True)
    ap.add_argument('--bag', required=True)
    ap.add_argument('--weight-format', choices=('th', 'hf-safetensors'), required=True)
    ap.add_argument('--weight', action='append', required=True)
    ap.add_argument('--device', choices=('cpu', 'cuda'), default='cpu')
    ap.add_argument('--in', dest='src', required=True)
    ap.add_argument('--outdir', required=True)
    a = ap.parse_args(argv)
    repo, bag, selected = demucs_binding(a.model, a.repo, a.bag, a.weight, a.weight_format)
    source = local_file(a.src)
    out = Path(a.outdir).resolve()
    with offline(), no_download_or_compile():
        from demucs.separate import main
        with demucs_local_loader(a.model, repo, bag, selected, a.weight_format):
            out.mkdir(parents=True, exist_ok=True)
            with tempfile.TemporaryDirectory(dir=out) as tmp:
                main(['--two-stems', 'vocals', '-n', a.model, '--repo', str(repo),
                      '--device', a.device, '-o', tmp, str(source)])
                stems = Path(tmp) / a.model / source.stem
                outputs = (('vocals.wav', 'vocals.wav'), ('no_vocals.wav', 'accompaniment.wav'))
                for src, _dst in outputs:
                    if not (stems / src).is_file():
                        raise RuntimeError('Demucs 产物缺失: ' + src)
                    verify_wav(stems / src, source, channels=2, rate=44100)
                for src, dst in outputs:
                    shutil.move(str(stems / src), str(out / dst))
    print('OK', out)
    return 0


def seed_binding(checkpoint, config, whisper, campplus, bigvgan):
    import yaml
    checkpoint, config, campplus = map(local_file, (checkpoint, config, campplus))
    whisper, bigvgan = map(local_directory, (whisper, bigvgan))
    cfg = yaml.safe_load(config.read_text(encoding='utf-8'))
    params = cfg['model_params']
    if (params['speech_tokenizer'] != {'type': 'whisper', 'name': 'openai/whisper-small'} or
            params['vocoder'] != {'type': 'bigvgan', 'name': 'nvidia/bigvgan_v2_22khz_80band_256x'} or
            params['DiT'].get('f0_condition') is not False or
            params['length_regulator'].get('f0_condition') is not False or
            params['length_regulator'].get('vector_quantize') is not False or
            cfg['preprocess_params']['sr'] != 22050 or
            cfg['preprocess_params']['spect_params']['hop_length'] != 256):
        raise ValueError('仅支持非 F0 Whisper Small / 22k BigVGAN 配套配置')
    # Explicit HF directory, no auto cache lookup. Sharded/other formats fail closed.
    for name in ('config.json', 'preprocessor_config.json'):
        local_file(whisper / name)
    weights = [whisper / name for name in ('model.safetensors', 'pytorch_model.bin')
               if (whisper / name).is_file()]
    if len(weights) != 1:
        raise ValueError('Seed Whisper 必须包含一个完整权重，暂不支持分片或多格式')
    local_file(weights[0])
    wcfg = json.loads((whisper / 'config.json').read_text(encoding='utf-8'))
    if (wcfg.get('model_type'), wcfg.get('d_model'), wcfg.get('encoder_layers')) != ('whisper', 768, 12):
        raise ValueError('Seed Whisper 不是受支持的 Small 架构')
    for name in ('config.json', 'bigvgan_generator.pt'):
        local_file(bigvgan / name)
    vcfg = json.loads((bigvgan / 'config.json').read_text(encoding='utf-8'))
    if (vcfg.get('sampling_rate'), vcfg.get('num_mels'), vcfg.get('hop_size')) != (22050, 80, 256):
        raise ValueError('BigVGAN 与非 F0 Seed 配置不兼容')
    params['speech_tokenizer']['name'] = str(whisper)
    params['vocoder']['name'] = str(bigvgan)
    params['style_encoder']['campplus_path'] = str(campplus)
    return checkpoint, cfg, whisper, campplus, bigvgan


@contextmanager
def seed_local_loaders(inference, checkpoint, whisper, campplus, bigvgan):
    """Narrow adapter for pinned inference.py's hardcoded CAMPPlus helper.

    Other models/configs fail closed. Wrappers force local_files_only and count
    dependency use. Patches are scoped to this single-job process and restored.
    """
    from transformers import WhisperModel, AutoFeatureExtractor
    from modules.bigvgan.bigvgan import BigVGAN
    used = set()

    def verify():
        if used != {'campplus', 'whisper', 'features', 'bigvgan', 'checkpoint'}:
            raise RuntimeError('Seed-VC 未实际使用全部显式依赖')

    def strict_checkpoint(model, optimizer, path, load_only_params=True,
                          ignore_modules=None, is_distributed=False):
        # Upstream commons.load_checkpoint silently skips missing/shape-mismatched
        # tensors. This fixed inference-only adapter must not report random weights
        # as a successful local checkpoint load.
        if (str(path) != str(checkpoint) or optimizer is not None or
                not load_only_params or ignore_modules or is_distributed):
            raise ValueError('仅支持绑定的 Seed 推理 checkpoint 加载')
        state = inference.torch.load(path, map_location='cpu', weights_only=True)['net']
        if set(state) != set(model):
            raise ValueError('Seed checkpoint 模块不完整或不兼容')
        for key, module in model.items():
            params = {name.removeprefix('module.'): value for name, value in state[key].items()}
            if len(params) != len(state[key]):
                raise ValueError('Seed checkpoint 键冲突')
            module.load_state_dict(params, strict=True)
            module.eval()
        used.add('checkpoint')
        return model, None, 0, 0

    def camp(repo_id, model_filename='pytorch_model.bin', config_filename=None):
        if (repo_id, model_filename, config_filename) != ('funasr/campplus', 'campplus_cn_common.bin', None):
            denied()
        used.add('campplus')
        return str(campplus)

    def local_loader(original, path, label, vocoder=False):
        def load(name, *args, **kwargs):
            if str(name) != str(path) or args:
                raise ValueError('禁止 Seed 默认或跨绑定依赖加载: ' + label)
            kwargs['local_files_only'] = True
            if vocoder:
                if kwargs.get('use_cuda_kernel', False):
                    denied()
                kwargs['use_cuda_kernel'] = False
            used.add(label)
            return original(str(path), **kwargs)
        return load

    with ExitStack() as stack:
        stack.enter_context(replaced(inference, 'load_custom_model_from_hf', camp))
        stack.enter_context(replaced(inference, 'load_checkpoint', strict_checkpoint))
        for cls, path, label, vocoder in ((WhisperModel, whisper, 'whisper', False),
                                         (AutoFeatureExtractor, whisper, 'features', False),
                                         (BigVGAN, bigvgan, 'bigvgan', True)):
            stack.enter_context(replaced(cls, 'from_pretrained', local_loader(
                cls.from_pretrained, path, label, vocoder)))
        yield verify
        verify()


def seedvc(argv):
    ap = argparse.ArgumentParser()
    for name in ('source', 'target', 'out', 'checkpoint', 'config', 'seed-whisper', 'campplus', 'bigvgan'):
        ap.add_argument('--' + name, required=True)
    ap.add_argument('--device', choices=('cpu', 'cuda'), required=True)
    ap.add_argument('--steps', type=int, choices=range(1, 201), default=25)
    # Non-F0 upstream ignores pitch; reject it rather than claim pitch conversion.
    ap.add_argument('--pitch', type=int, choices=(0,), default=0)
    a = ap.parse_args(argv)
    checkpoint, cfg, whisper, campplus, bigvgan = seed_binding(
        a.checkpoint, a.config, a.seed_whisper, a.campplus, a.bigvgan)
    source, target = local_file(a.source), local_file(a.target)
    out = Path(a.out).resolve()
    resources = Path(getattr(sys, '_MEIPASS', Path(__file__).parent)) / 'seedvc'
    if not resources.is_dir():
        raise RuntimeError('Seed-VC 冻结资源缺失，待构建')
    with offline(), no_download_or_compile():
        # Fixed import only; build collects this pinned alias and resource tree.
        import seedvc_inference
        import torch
        import yaml
        if a.device == 'cuda' and not torch.cuda.is_available():
            raise RuntimeError('绑定的 CUDA 设备不可用')
        with replaced(seedvc_inference, 'device', torch.device(a.device)), seed_local_loaders(
                seedvc_inference, checkpoint, whisper, campplus, bigvgan) as verify:
            out.parent.mkdir(parents=True, exist_ok=True)
            with tempfile.TemporaryDirectory(dir=out.parent) as tmp:
                local_config = Path(tmp) / 'bound-config.yml'
                local_config.write_text(yaml.safe_dump(cfg), encoding='utf-8')
                args = argparse.Namespace(source=str(source), target=str(target), output=tmp,
                    diffusion_steps=a.steps, length_adjust=1.0, inference_cfg_rate=0.7,
                    f0_condition=False, auto_f0_adjust=False, semi_tone_shift=0, fp16=a.device == 'cuda',
                    checkpoint=str(checkpoint), config=str(local_config))
                # Used inputs are absolute; never chdir into source defaults/cache.
                seedvc_inference.main(args)
                verify()  # Verify before publishing the output, not just on scope exit.
                produced = list(Path(tmp).glob('vc_*.wav'))
                if len(produced) != 1:
                    raise RuntimeError('Seed-VC WAV 产物缺失或不唯一')
                verify_wav(produced[0], source, channels=1, rate=22050)
                shutil.move(str(produced[0]), str(out))
    print('OK', out)
    return 0


def main(argv=None, fixed_id=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    identity = fixed_id or component_id()
    if identity not in ("demucs-runner", "seedvc-runner"):
        raise RuntimeError("仅支持构建期固定的组件入口")
    if argv == ["--component-info"]:
        print(json.dumps({"schema": 1, "component_id": identity,
            "packaging": "pyinstaller-onedir", "protocols": [{
                "demucs-runner": "fengshen.demucs.cli/v1",
                "seedvc-runner": "fengshen.seedvc.cli/v1",
            }[identity]], "inference": "not_tested"}))
        return 0
    return demucs(argv) if identity == "demucs-runner" else seedvc(argv)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)
