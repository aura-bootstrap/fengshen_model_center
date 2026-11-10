"""Directed offline contracts; no downloaded weights or inference claims."""
import ast
import contextlib
import json
import types
import importlib.util
import io
from pathlib import Path
import socket
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("frozen_runner", HERE / "model_center_frozen_runner.py")
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class EntryTests(unittest.TestCase):
    def test_metadata_never_imports_models_even_when_frozen(self):
        import builtins
        original = builtins.__import__

        def checked(name, *args, **kwargs):
            if name.split('.')[0] in {'torch', 'demucs', 'seedvc_inference', 'transformers'}:
                self.fail('metadata imported ' + name)
            return original(name, *args, **kwargs)

        with patch.object(sys, 'frozen', True, create=True), patch.object(sys, 'executable', 'seedvc-runner.exe'), patch.object(builtins, '__import__', checked), contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(runner.main(['--component-info']), 0)
        self.assertIn('not_tested', out.getvalue())

    def test_metadata_declares_only_fixed_cli_protocol_without_inference_imports(self):
        import builtins
        original = builtins.__import__

        def checked(name, *args, **kwargs):
            if name.split('.')[0] in {'torch', 'demucs', 'seedvc_inference', 'transformers',
                                      'huggingface_hub', 'yaml', 'modules'}:
                self.fail('metadata imported ' + name)
            return original(name, *args, **kwargs)

        for identity, protocol in (('demucs-runner', 'fengshen.demucs.cli/v1'),
                                   ('seedvc-runner', 'fengshen.seedvc.cli/v1')):
            for frozen in (False, True):
                with self.subTest(identity=identity, frozen=frozen), contextlib.ExitStack() as stack:
                    stack.enter_context(patch.object(sys, 'frozen', frozen, create=True))
                    stack.enter_context(patch.object(sys, 'executable', identity + '.exe'))
                    stack.enter_context(patch.object(sys, 'argv', [identity + '.py']))
                    stack.enter_context(patch.object(builtins, '__import__', checked))
                    stack.enter_context(patch.object(runner, 'demucs', side_effect=AssertionError('inference dispatch')))
                    stack.enter_context(patch.object(runner, 'seedvc', side_effect=AssertionError('inference dispatch')))
                    out = stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
                    self.assertEqual(runner.main(['--component-info']), 0)
                    self.assertEqual(json.loads(out.getvalue()), {
                        'schema': 1, 'component_id': identity, 'packaging': 'pyinstaller-onedir',
                        'protocols': [protocol], 'inference': 'not_tested'})

    def test_metadata_rejects_unknown_fixed_component(self):
        for identity in ('unknown-runner', 'demucs-runner/v1', 'seedvc', 'DEMUCS-RUNNER'):
            with self.subTest(identity=identity), contextlib.redirect_stdout(io.StringIO()) as out:
                with self.assertRaisesRegex(RuntimeError, '固定'):
                    runner.main(['--component-info'], fixed_id=identity)
                self.assertEqual(out.getvalue(), '')

    def test_missing_bindings_fail_before_model_import(self):
        for identity, args in (
            ('demucs-runner', ['--in', 'source.wav', '--outdir', 'out']),
            ('seedvc-runner', ['--source', 'source.wav', '--target', 'ref.wav', '--out', 'out.wav']),
        ):
            with self.subTest(identity=identity), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                runner.main(args, fixed_id=identity)

    def test_seed_requires_explicit_cpu_or_cuda_before_binding_or_import(self):
        argv = [item for name in ('source', 'target', 'out', 'checkpoint', 'config',
                                  'seed-whisper', 'campplus', 'bigvgan')
                for item in ('--' + name, 'unused')]
        for device in ([], ['--device', ''], ['--device', 'auto'], ['--device', 'mps'],
                       ['--device', 'CUDA'], ['--device', 'cuda:0']):
            with self.subTest(device=device), contextlib.redirect_stderr(io.StringIO()), patch.object(
                    runner, 'seed_binding', side_effect=AssertionError('binding reached')) as binding:
                with self.assertRaises(SystemExit) as error:
                    runner.seedvc(argv + device)
                self.assertEqual(error.exception.code, 2)
                binding.assert_not_called()

    def test_offline_guard_is_scoped_and_blocks_dns_and_connections(self):
        original = socket.create_connection
        with runner.offline():
            for action in (lambda: socket.create_connection(('example.invalid', 443)),
                           lambda: socket.getaddrinfo('example.invalid', 443),
                           lambda: socket.gethostbyname('example.invalid')):
                with self.assertRaises(RuntimeError):
                    action()
        self.assertIs(socket.create_connection, original)


class LocalBindingTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def put(self, path, content=b'fixture-not-a-real-weight'):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content)
        return path

    def wav(self, path, channels=1, rate=22050, frames=2205):
        import wave
        path.parent.mkdir(parents=True, exist_ok=True)
        with wave.open(str(path), 'wb') as audio:
            audio.setnchannels(channels)
            audio.setsampwidth(2)
            audio.setframerate(rate)
            audio.writeframes(b'\0' * frames * channels * 2)
        return path

    def test_wav_contract_rejects_wrong_rate_channels_duration_and_truncation(self):
        source = self.wav(self.root / 'source.wav', frames=22050)
        output = self.wav(self.root / 'output.wav', frames=22050)
        runner.verify_wav(output, source, 1, 22050)
        for channels, rate, frames in ((2, 22050, 22050), (1, 44100, 44100), (1, 22050, 100)):
            self.wav(output, channels, rate, frames)
            with self.assertRaises(ValueError):
                runner.verify_wav(output, source, 1, 22050)
        self.wav(output)
        output.write_bytes(output.read_bytes()[:-10])
        with self.assertRaises(ValueError):
            runner.wav_info(output)

    def demucs_group(self, model, fmt):
        import yaml
        repo = self.root / (model + fmt)
        repo.mkdir()
        bag = {'models': runner.DEMUCS_GROUPS[model]}
        if model.endswith('_ft'):
            bag['weights'] = [[float(i == j) for j in range(4)] for i in range(4)]
        component = self.root / ('component-' + model + fmt)
        bag_path = self.put(component / 'resources/demucs' / (model + '.yaml'), yaml.safe_dump(bag).encode())
        # Package-internal injection only; the public CLI cannot choose a root.
        self.enterContext(patch.object(runner, '__file__', str(component / 'model_center_frozen_runner.py')))
        weights = [self.put(repo / (sig + ('.th' if fmt == 'th' else '.safetensors')))
                   for sig in bag['models']]
        return repo, bag_path, weights

    def test_demucs_both_complete_groups_and_explicit_formats(self):
        for model in runner.DEMUCS_GROUPS:
            for fmt in ('th', 'hf-safetensors'):
                with self.subTest(model=model, fmt=fmt):
                    repo, bag, weights = self.demucs_group(model, fmt)
                    bound = runner.demucs_binding(model, repo, bag, weights, fmt)
                    self.assertEqual(list(bound[2]), runner.DEMUCS_GROUPS[model])
                    with self.assertRaises(ValueError):
                        runner.demucs_binding(model, repo, bag, weights[:-1], fmt)
                    with self.assertRaises(ValueError):
                        runner.demucs_binding(model, repo, bag, weights + weights[:1], fmt)
                    with self.assertRaises(ValueError):
                        runner.demucs_binding(model, repo, bag, weights, 'th' if fmt != 'th' else 'hf-safetensors')

    def test_build_stages_only_locked_official_bags_and_rechecks_before_manifest(self):
        import hashlib
        import yaml
        script = (HERE.parent / 'installer/build_model_center.ps1').read_text(encoding='utf-8')
        code = script.split("$demucsBags = @'\n", 1)[1].split("\n'@", 1)[0]
        inputs = self.root / 'inputs'
        package = inputs / 'python/Lib/site-packages/demucs'
        pins = []
        for model, signatures in runner.DEMUCS_GROUPS.items():
            bag = {'models': signatures}
            if model.endswith('_ft'):
                bag['weights'] = [[float(i == j) for j in range(4)] for i in range(4)]
            path = self.put(package / 'remote' / (model + '.yaml'), yaml.safe_dump(bag).encode())
            pins.append({'path': path.relative_to(inputs).as_posix(),
                         'sha256': hashlib.sha256(path.read_bytes()).hexdigest()})
        lock = self.put(self.root / 'lock.json', json.dumps({'inputs': pins}).encode())
        stage = self.root / 'stage'

        def run(mode):
            with patch.object(sys, 'argv', ['bags', str(inputs), str(lock), str(stage), mode]), patch.object(importlib.util, 'find_spec', return_value=types.SimpleNamespace(origin=str(package / '__init__.py'))):
                exec(compile(code, 'build_model_center.ps1:demucsBags', 'exec'), {})

        run('stage')
        run('verify')
        fixed = stage / 'resources/demucs/htdemucs_ft.yaml'
        fixed.write_text('models: []', encoding='utf-8')
        with self.assertRaises(RuntimeError):
            run('verify')
        # A valid hash alone cannot authorize an invalid FT mixing matrix.
        source = package / 'remote/htdemucs_ft.yaml'
        bad = {'models': runner.DEMUCS_GROUPS['htdemucs_ft'], 'weights': [[1.] * 4] * 4}
        source.write_text(yaml.safe_dump(bad), encoding='utf-8')
        pins[1]['sha256'] = hashlib.sha256(source.read_bytes()).hexdigest()
        lock.write_text(json.dumps({'inputs': pins}), encoding='utf-8')
        with self.assertRaises(RuntimeError):
            run('stage')
        pins.pop()
        lock.write_text(json.dumps({'inputs': pins}), encoding='utf-8')
        with self.assertRaises(RuntimeError):
            run('stage')
        verify_at = script.index("$dir,'verify'")
        publish_at = script.index("Metadata-Smoke $id $dir ($id + '.exe') 'pyinstaller-onedir'")
        self.assertLess(verify_at, publish_at)
        metadata_helper = script.split('function Metadata-Smoke(', 1)[1].split('\n$centerDir =', 1)[0]
        self.assertIn('Manifest $Id $Directory $Entry $Packaging', metadata_helper)

    def test_demucs_rejects_shadow_and_wrong_bag(self):
        repo, bag, weights = self.demucs_group('htdemucs', 'th')
        self.put(repo / '955717e8-deadbeef.th')
        with self.assertRaises(ValueError):
            runner.demucs_binding('htdemucs', repo, bag, weights, 'th')
        bag.write_text("models: ['f7e0c4bc']", encoding='utf-8')
        with self.assertRaises(ValueError):
            runner.demucs_binding('htdemucs', repo, bag, weights, 'th')

    def test_demucs_rejects_repo_yaml_and_frozen_root_override(self):
        repo, bag, weights = self.demucs_group('htdemucs', 'th')
        neighbor = self.put(repo / 'htdemucs.yaml', bag.read_bytes())
        with self.assertRaises(ValueError):
            runner.demucs_binding('htdemucs', repo, neighbor, weights, 'th')
        frozen = self.root / 'frozen'
        fixed = self.put(frozen / 'resources/demucs/htdemucs.yaml', bag.read_bytes())
        with patch.object(sys, 'frozen', True, create=True), patch.object(sys, 'executable', str(frozen / 'demucs-runner.exe')):
            runner.demucs_binding('htdemucs', repo, fixed, weights, 'th')
            with self.assertRaises(ValueError):
                runner.demucs_binding('htdemucs', repo, bag, weights, 'th')
            fixed.unlink()
            with self.assertRaises(ValueError):
                runner.demucs_binding('htdemucs', repo, neighbor, weights, 'th')

    def test_th_loader_preserves_checksum_and_rejects_default_or_cross_binding(self):
        import demucs.api
        from demucs.repo import ModelLoadingError
        repo, bag_path, weights = self.demucs_group('htdemucs', 'th')
        weights[0] = weights[0].rename(repo / '955717e8-deadbeef.th')
        repo, bag, selected = runner.demucs_binding('htdemucs', repo, bag_path, weights, 'th')
        original = demucs.api.get_model
        with runner.offline(), runner.demucs_local_loader('htdemucs', repo, bag, selected, 'th'):
            for name, directory in (('htdemucs', None), ('htdemucs_ft', repo), ('htdemucs', self.root)):
                with self.assertRaises(ValueError):
                    demucs.api.get_model(name, repo=directory)
            with self.assertRaisesRegex(ModelLoadingError, 'Invalid checksum'):
                demucs.api.get_model('htdemucs', repo=repo)
        self.assertIs(demucs.api.get_model, original)

    def test_demucs_calls_real_parser_with_local_repo_not_default(self):
        import demucs.separate
        repo, bag, weights = self.demucs_group('htdemucs', 'th')
        source = self.wav(self.root / 'source.wav')
        output = self.root / 'output'

        def separate(argv):
            args = demucs.separate.get_parser().parse_args(argv)
            self.assertEqual(args.repo, repo)
            self.assertEqual(args.name, 'htdemucs')
            self.assertEqual(args.stem, 'vocals')
            stems = args.out / args.name / source.stem
            self.wav(stems / 'vocals.wav', channels=2, rate=44100, frames=4410)
            self.wav(stems / 'no_vocals.wav', channels=2, rate=44100, frames=4410)

        args = ['--model', 'htdemucs', '--repo', str(repo), '--bag', str(bag),
                '--weight-format', 'th', '--weight', str(weights[0]),
                '--in', str(source), '--outdir', str(output)]
        with patch.object(demucs.separate, 'main', separate):
            self.assertEqual(runner.demucs(args), 0)
        self.assertTrue((output / 'vocals.wav').is_file())
        self.assertTrue((output / 'accompaniment.wav').is_file())

    def test_hf_adapter_uses_real_demucs_decoder_and_rejects_invalid_format(self):
        import demucs.api
        repo, bag_path, weights = self.demucs_group('htdemucs', 'hf-safetensors')
        repo, bag, selected = runner.demucs_binding('htdemucs', repo, bag_path, weights, 'hf-safetensors')
        # Invalid safetensors must actually fail decoding, never be counted as loaded.
        with runner.offline(), runner.no_download_or_compile(), runner.demucs_local_loader('htdemucs', repo, bag, selected, 'hf-safetensors'):
            with self.assertRaises(ValueError):
                demucs.api.get_model('htdemucs', repo=None)
            with self.assertRaises(Exception):
                demucs.api.get_model('htdemucs', repo=repo)

    def test_native_th_and_hf_loaders_deserialize_tiny_fixture_bags_offline(self):
        # Actual Demucs codecs + BagOfModels, tiny randomly initialized HDemucs.
        # Serialization compatibility only: not production HTDemucs inference.
        import torch
        import demucs.api
        from demucs.pretrained import demucs_unittest
        from safetensors.torch import save_file
        model = demucs_unittest()
        kwargs = {'channels': 4, 'sources': ['drums', 'bass', 'other', 'vocals']}
        for fmt in ('th', 'hf-safetensors'):
            for name in runner.DEMUCS_GROUPS:
                repo, bag_path, weights = self.demucs_group(name, fmt)
                for weight in weights:
                    if fmt == 'th':
                        torch.save({'klass': type(model), 'args': [], 'kwargs': kwargs,
                                    'state': model.state_dict()}, weight)
                    else:
                        save_file(model.state_dict(), str(weight), metadata={
                            'klass': 'demucs.hdemucs.HDemucs', 'args': '[]',
                            'kwargs': json.dumps(kwargs)})
                repo, bag, selected = runner.demucs_binding(name, repo, bag_path, weights, fmt)
                with runner.offline(), runner.no_download_or_compile(), runner.demucs_local_loader(name, repo, bag, selected, fmt):
                    loaded = demucs.api.get_model(name, repo=repo)
                    self.assertEqual(len(loaded.models), len(weights))
                    self.assertEqual(loaded.sources, kwargs['sources'])
                    self.assertEqual(loaded.samplerate, model.samplerate)
                    self.assertTrue(torch.equal(next(loaded.models[0].parameters()), next(model.parameters())))

    def seed_group(self):
        config = HERE / 'seed-vc/configs/presets/config_dit_mel_seed_uvit_whisper_small_wavenet.yml'
        checkpoint = self.put(self.root / 'seed.pth')
        camp = self.put(self.root / 'camp.bin')
        whisper, vocoder = self.root / 'whisper', self.root / 'bigvgan'
        self.put(whisper / 'config.json', json.dumps({'model_type': 'whisper', 'd_model': 768, 'encoder_layers': 12}).encode())
        self.put(whisper / 'preprocessor_config.json', b'{}')
        self.put(whisper / 'model.safetensors')
        self.put(vocoder / 'config.json', json.dumps({'sampling_rate': 22050, 'num_mels': 80, 'hop_size': 256}).encode())
        self.put(vocoder / 'bigvgan_generator.pt')
        return checkpoint, config, whisper, camp, vocoder

    def test_seed_config_rewrites_all_dependencies_and_rejects_missing_or_f0(self):
        import yaml
        args = self.seed_group()
        checkpoint, cfg, whisper, camp, vocoder = runner.seed_binding(*args)
        self.assertEqual(cfg['model_params']['speech_tokenizer']['name'], str(whisper))
        self.assertEqual(cfg['model_params']['vocoder']['name'], str(vocoder))
        self.assertEqual(cfg['model_params']['style_encoder']['campplus_path'], str(camp))
        bad = self.root / 'f0.yml'
        cfg['model_params']['DiT']['f0_condition'] = True
        self.put(bad, yaml.safe_dump(cfg).encode())
        with self.assertRaises(ValueError):
            runner.seed_binding(checkpoint, bad, whisper, camp, vocoder)
        for path in (camp, whisper / 'model.safetensors', vocoder / 'bigvgan_generator.pt'):
            content = path.read_bytes()
            path.write_bytes(b'')
            with self.assertRaises(ValueError):
                runner.seed_binding(*args)
            path.write_bytes(content)

    def test_seed_binding_rejects_vector_quantize_before_optional_imports(self):
        import yaml
        checkpoint, config, whisper, camp, vocoder = self.seed_group()
        original = yaml.safe_load(config.read_text(encoding='utf-8'))
        for value in (True, None, 0, 'false'):
            with self.subTest(value=value):
                cfg = json.loads(json.dumps(original))
                if value is None:
                    cfg['model_params']['length_regulator'].pop('vector_quantize')
                else:
                    cfg['model_params']['length_regulator']['vector_quantize'] = value
                bad = self.root / 'quantized.yml'
                self.put(bad, yaml.safe_dump(cfg).encode())
                with self.assertRaises(ValueError):
                    runner.seed_binding(checkpoint, bad, whisper, camp, vocoder)

    def seed_source_harness(self, checkpoint, camp):
        """Execute the actual upstream load_models body, replacing only heavy
        neural objects. This is wiring evidence, explicitly not real inference.
        """
        from unittest.mock import MagicMock
        import torch
        import yaml
        from munch import Munch
        source = (HERE / 'seed-vc/inference.py').read_text(encoding='utf-8')
        tree = ast.parse(source)
        function = next(node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name == 'load_models')
        module = types.ModuleType('seedvc_inference')
        module.__dict__.update(yaml=yaml, device='cpu', fp16=False, torch=torch)
        module.load_custom_model_from_hf = runner.denied
        module.load_checkpoint = runner.denied
        module.recursive_munch = lambda d: Munch.fromDict(d)
        module.build_model = lambda *a, **k: Munch(cfm=MagicMock(), length_regulator=MagicMock())
        exec(compile(ast.Module(body=[function], type_ignores=[]), 'seed-vc/inference.py', 'exec'), module.__dict__)
        modules = {}
        for name in ('modules', 'modules.campplus', 'modules.campplus.DTDNN', 'modules.bigvgan',
                     'modules.bigvgan.bigvgan', 'modules.audio', 'transformers'):
            modules[name] = types.ModuleType(name)
        modules['modules.campplus.DTDNN'].CAMPPlus = MagicMock()
        modules['modules.audio'].mel_spectrogram = MagicMock()
        modules['modules.bigvgan'].bigvgan = modules['modules.bigvgan.bigvgan']
        for name, attr in (('modules.bigvgan.bigvgan', 'BigVGAN'),
                           ('transformers', 'WhisperModel'), ('transformers', 'AutoFeatureExtractor')):
            # Real classes retain descriptor restoration semantics, no models loaded.
            class Loader:
                calls = []

                @classmethod
                def from_pretrained(cls, name, **kwargs):
                    cls.calls.append((name, kwargs))
                    return MagicMock()
            setattr(modules[name], attr, Loader)
        state = {'net': {'cfm': {}, 'length_regulator': {}}}
        torch.save(state, checkpoint)
        torch.save({}, camp)
        return module, modules

    def test_actual_seed_load_models_uses_only_bound_paths_and_no_cuda_kernel(self):
        import yaml
        args = self.seed_group()
        checkpoint, cfg, whisper, camp, vocoder = runner.seed_binding(*args)
        config = self.put(self.root / 'bound.yml', yaml.safe_dump(cfg).encode())
        module, modules = self.seed_source_harness(checkpoint, camp)
        original = modules['transformers'].WhisperModel.from_pretrained
        with patch.dict(sys.modules, modules), runner.offline(), runner.no_download_or_compile():
            with runner.seed_local_loaders(module, checkpoint, whisper, camp, vocoder) as verify:
                result = module.load_models(types.SimpleNamespace(fp16=True, f0_condition=False,
                                                                  checkpoint=str(checkpoint), config=str(config)))
                verify()
                self.assertIsNone(result[2])
                for cls, path in ((modules['transformers'].WhisperModel, whisper),
                                  (modules['transformers'].AutoFeatureExtractor, whisper),
                                  (modules['modules.bigvgan.bigvgan'].BigVGAN, vocoder)):
                    self.assertEqual(cls.calls[0][0], str(path))
                    self.assertTrue(cls.calls[0][1]['local_files_only'])
                self.assertIs(modules['modules.bigvgan.bigvgan'].BigVGAN.calls[0][1]['use_cuda_kernel'], False)
                with self.assertRaises(RuntimeError):
                    module.load_custom_model_from_hf('Plachta/Seed-VC', 'fallback.pth')
                with self.assertRaises(ValueError):
                    modules['transformers'].WhisperModel.from_pretrained('openai/whisper-small')
                with self.assertRaises(RuntimeError):
                    modules['modules.bigvgan.bigvgan'].BigVGAN.from_pretrained(str(vocoder), use_cuda_kernel=True)
        self.assertEqual(modules['transformers'].WhisperModel.from_pretrained, original)

    def test_seed_strict_checkpoint_rejects_partial_and_cross_binding(self):
        import torch
        args = self.seed_group()
        checkpoint, cfg, whisper, camp, vocoder = runner.seed_binding(*args)
        module, modules = self.seed_source_harness(checkpoint, camp)
        # A real tiny torch layer makes partial-state rejection observable.
        torch.save({'net': {'layer': {}}}, checkpoint)
        with patch.dict(sys.modules, modules), runner.offline(), runner.no_download_or_compile():
            with self.assertRaises(RuntimeError), runner.seed_local_loaders(module, checkpoint, whisper, camp, vocoder):
                module.load_checkpoint({'layer': torch.nn.Linear(1, 1)}, None, str(checkpoint))
            with self.assertRaises(ValueError), runner.seed_local_loaders(module, checkpoint, whisper, camp, vocoder):
                module.load_checkpoint({}, None, str(self.root / 'other.pth'))
            with self.assertRaises(RuntimeError), runner.seed_local_loaders(module, checkpoint, whisper, camp, vocoder):
                pass  # A fake inference result must not publish without dependency use.

    def test_seed_entry_passes_checkpoint_config_and_fixed_non_f0_parameters(self):
        for device in ('cpu', 'cuda'):
            with self.subTest(device=device):
                self.seed_entry(device)

    def test_seed_entry_restores_device_and_loaders_after_failure(self):
        for device in ('cpu', 'cuda'):
            with self.subTest(device=device):
                self.seed_entry(device, fail=True)

    def test_seed_entry_rejects_unavailable_cuda_without_cpu_fallback(self):
        self.seed_entry('cuda', available=False)

    def seed_entry(self, device, fail=False, available=True):
        import torch
        import yaml
        args = self.seed_group()
        checkpoint, config, whisper, camp, vocoder = args
        module, modules = self.seed_source_harness(checkpoint, camp)
        source, target = self.wav(self.root / 'source.wav'), self.wav(self.root / 'ref.wav')
        out = self.root / 'output.wav'
        resources = self.root / 'frozen'
        (resources / 'seedvc').mkdir(parents=True, exist_ok=True)
        previous = Path.cwd()
        original_device = module.device = torch.device('cuda' if available else 'cpu')
        original_camp, original_checkpoint = module.load_custom_model_from_hf, module.load_checkpoint
        loaders = [(getattr(modules[name], attr), getattr(modules[name], attr).__dict__['from_pretrained'])
                   for name, attr in (('transformers', 'WhisperModel'),
                                      ('transformers', 'AutoFeatureExtractor'),
                                      ('modules.bigvgan.bigvgan', 'BigVGAN'))]
        calls = []

        def main(a):
            calls.append(a)
            self.assertEqual(module.device, torch.device(device))
            self.assertIs(a.fp16, device == 'cuda')
            self.assertEqual(a.checkpoint, str(checkpoint))
            self.assertTrue(Path(a.config).is_absolute())
            self.assertEqual(a.length_adjust, 1.0)
            self.assertIs(a.f0_condition, False)
            self.assertIs(a.auto_f0_adjust, False)
            self.assertEqual(a.semi_tone_shift, 0)
            self.assertEqual(Path.cwd(), previous)
            bound = yaml.safe_load(Path(a.config).read_text(encoding='utf-8'))
            self.assertEqual(bound['model_params']['vocoder']['name'], str(vocoder))
            loaded = module.load_models(a)
            self.assertIs(module.fp16, device == 'cuda')
            for model in loaded[0].values():
                model.to.assert_called_once_with(torch.device(device))
            modules['modules.campplus.DTDNN'].CAMPPlus.return_value.to.assert_called_once_with(torch.device(device))
            if fail:
                raise RuntimeError('fixture inference failure')
            self.wav(Path(a.output) / 'vc_fixture.wav')

        module.main = main
        modules['seedvc_inference'] = module
        argv = ['--source', str(source), '--target', str(target), '--out', str(out),
                '--checkpoint', str(checkpoint), '--config', str(config),
                '--seed-whisper', str(whisper), '--campplus', str(camp), '--bigvgan', str(vocoder),
                '--device', device]
        out.unlink(missing_ok=True)
        with patch.dict(sys.modules, modules), patch.object(sys, '_MEIPASS', str(resources), create=True), patch.object(
                torch.cuda, 'is_available', return_value=available):
            if not available:
                with self.assertRaisesRegex(RuntimeError, 'CUDA'):
                    runner.seedvc(argv)
                self.assertEqual(calls, [])
            elif fail:
                with self.assertRaisesRegex(RuntimeError, 'fixture inference failure'):
                    runner.seedvc(argv)
                self.assertEqual(len(calls), 1)
            else:
                self.assertEqual(runner.seedvc(argv), 0)
                self.assertEqual(len(calls), 1)
        self.assertEqual(out.is_file(), available and not fail)
        self.assertIs(module.device, original_device)
        self.assertIs(module.load_custom_model_from_hf, original_camp)
        self.assertIs(module.load_checkpoint, original_checkpoint)
        for cls, descriptor in loaders:
            self.assertIs(cls.__dict__['from_pretrained'], descriptor)
        self.assertEqual(Path.cwd(), previous)
        self.assertFalse((resources / 'seedvc/checkpoints').exists())
        # The source preset and all supplied weight fixtures remain unchanged.
        self.assertEqual(config, HERE / 'seed-vc/configs/presets/config_dit_mel_seed_uvit_whisper_small_wavenet.yml')

    def test_actual_bigvgan_local_loader_never_downloads_or_enables_cuda(self):
        import os
        import torch
        args = self.seed_group()
        vocoder = args[-1]
        torch.save({'generator': {}}, vocoder / 'bigvgan_generator.pt')
        source = (HERE / 'seed-vc/modules/bigvgan/bigvgan.py').read_text(encoding='utf-8')
        tree = ast.parse(source)
        klass = next(n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == 'BigVGAN')
        fn = next(n for n in klass.body if isinstance(n, ast.FunctionDef) and n.name == '_from_pretrained')
        fn.decorator_list = []
        namespace = {'os': os, 'torch': torch, 'hf_hub_download': runner.denied,
                     'load_hparams_from_json': lambda p: json.loads(Path(p).read_text(encoding='utf-8'))}
        # Postponed annotations avoid importing typing / model code for this harness.
        code = ast.Module(body=[ast.ImportFrom(module='__future__', names=[ast.alias(name='annotations')], level=0), fn], type_ignores=[])
        ast.fix_missing_locations(code)
        exec(compile(code, 'modules/bigvgan/bigvgan.py', 'exec'), namespace)

        class LocalVocoder:
            def __init__(self, h, use_cuda_kernel=False):
                self.h, self.cuda = h, use_cuda_kernel

            def load_state_dict(self, state):
                if state != {}:
                    raise AssertionError('unexpected weights')

        with runner.offline(), runner.no_download_or_compile():
            model = namespace['_from_pretrained'](LocalVocoder, model_id=str(vocoder), revision=None,
                cache_dir=None, force_download=False, proxies=None, resume_download=None,
                local_files_only=True, token=None, use_cuda_kernel=False)
        self.assertIs(model.cuda, False)
        self.assertEqual(model.h['sampling_rate'], 22050)

    def test_seed_fake_main_without_dependency_load_cannot_publish(self):
        args = self.seed_group()
        checkpoint, config, whisper, camp, vocoder = args
        module, modules = self.seed_source_harness(checkpoint, camp)
        source, target = self.wav(self.root / 'source.wav'), self.wav(self.root / 'ref.wav')
        out = self.root / 'must-not-exist.wav'
        resources = self.root / 'frozen'
        (resources / 'seedvc').mkdir(parents=True)
        module.main = lambda a: self.put(Path(a.output) / 'vc_fake.wav')
        modules['seedvc_inference'] = module
        argv = ['--source', str(source), '--target', str(target), '--out', str(out),
                '--checkpoint', str(checkpoint), '--config', str(config),
                '--seed-whisper', str(whisper), '--campplus', str(camp), '--bigvgan', str(vocoder),
                '--device', 'cpu']
        with patch.dict(sys.modules, modules), patch.object(sys, '_MEIPASS', str(resources), create=True), self.assertRaises(RuntimeError):
            runner.seedvc(argv)
        self.assertFalse(out.exists())

    def test_download_and_compile_helpers_are_denied_without_network(self):
        import torch
        import torch.utils.cpp_extension as ext
        import huggingface_hub as hf
        with runner.offline(), runner.no_download_or_compile():
            for call in (lambda: hf.hf_hub_download('repo', 'model'),
                         lambda: hf.snapshot_download('repo'),
                         lambda: torch.hub.load_state_dict_from_url('https://invalid'),
                         lambda: ext.load('kernel', []), lambda: ext.load_inline('kernel', ''),
                         lambda: torch.compile(lambda x: x)):
                with self.assertRaises(RuntimeError):
                    call()

    def test_no_subprocess_or_python_interpreter_launch_in_entry(self):
        tree = ast.parse((HERE / 'model_center_frozen_runner.py').read_text(encoding='utf-8'))
        self.assertFalse(any(isinstance(node, ast.Import) and any(alias.name == 'subprocess' for alias in node.names)
                             for node in ast.walk(tree)))
        self.assertFalse(any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) and
                             node.func.attr in {'system', 'popen', 'Popen'} for node in ast.walk(tree)))


class SeedOptionalImportTests(unittest.TestCase):
    """Source execution with controlled dependencies, not inference or closure proof."""

    def source_module(self, relative, plotting=False):
        import builtins
        import math
        from unittest.mock import MagicMock

        nn = MagicMock()
        nn.Module = object
        nn.ModuleList = list
        torch = MagicMock(nn=nn)
        plt = MagicMock()
        fig, ax = MagicMock(), MagicMock()
        plt.subplots.return_value = (fig, ax)
        events = []
        matplotlib = types.SimpleNamespace(
            use=lambda backend: events.append(('backend', backend)), pylab=plt)
        dependencies = {
            'torch': torch, 'torch.nn': nn, 'torch.nn.utils': MagicMock(),
            'modules.commons': MagicMock(), 'numpy': types.SimpleNamespace(log=math.log),
            'meldataset': types.SimpleNamespace(MAX_WAV_VALUE=32768),
            'scipy.io.wavfile': MagicMock(),
        }
        original = builtins.__import__

        def controlled(name, globals=None, locals=None, fromlist=(), level=0):
            if name.split('.')[0] in {'dac', 'audiotools', 'matplotlib'}:
                events.append(('import', name))
                if plotting and name.startswith('matplotlib'):
                    return matplotlib
                raise ModuleNotFoundError('blocked optional dependency: ' + name, name=name)
            if name == 'torch.nn' and not fromlist:
                return torch
            if name in dependencies:
                return dependencies[name]
            return original(name, globals, locals, fromlist, level)

        namespace = {'__builtins__': dict(vars(builtins), __import__=controlled)}
        path = HERE / 'seed-vc/modules' / relative
        exec(compile(path.read_text(encoding='utf-8'), str(path), 'exec'), namespace)
        return namespace, events, plt, fig, ax

    def test_non_f0_length_regulator_import_and_init_without_optional_dependencies(self):
        module, events, _, _, _ = self.source_module('length_regulator.py')
        model = module['InterpolateRegulator'](
            channels=8, sampling_ratios=[1], in_channels=8,
            is_discrete=False, vector_quantize=False, f0_condition=False)
        self.assertFalse(model.f0_condition)
        self.assertFalse(hasattr(model, 'vq'))
        self.assertEqual(events, [])

    def test_vector_quantize_still_attempts_original_dependency(self):
        module, events, _, _, _ = self.source_module('length_regulator.py')
        with self.assertRaisesRegex(ModuleNotFoundError, 'dac.nn.quantize') as error:
            module['InterpolateRegulator'](
                channels=8, sampling_ratios=[], in_channels=8,
                is_discrete=False, vector_quantize=True, f0_condition=False)
        self.assertEqual(error.exception.name, 'dac.nn.quantize')
        self.assertEqual(events, [('import', 'dac.nn.quantize')])

    def test_bigvgan_helpers_import_without_plotting_dependencies(self):
        from unittest.mock import MagicMock
        module, events, _, _, _ = self.source_module('bigvgan/utils.py')
        conv = type('Conv1d', (), {})()
        conv.weight = MagicMock()
        module['init_weights'](conv)
        conv.weight.data.normal_.assert_called_once_with(0.0, 0.01)
        self.assertEqual(module['get_padding'](3, 2), 2)
        self.assertEqual(events, [])
        for name in ('plot_spectrogram', 'plot_spectrogram_clipped'):
            with self.subTest(name=name), self.assertRaisesRegex(ModuleNotFoundError, 'matplotlib'):
                module[name](object())
        self.assertEqual(events, [('import', 'matplotlib')] * 2)

    def test_plot_calls_import_matplotlib_select_agg_and_preserve_rendering(self):
        for name in ('plot_spectrogram', 'plot_spectrogram_clipped'):
            with self.subTest(name=name):
                module, events, plt, fig, ax = self.source_module('bigvgan/utils.py', plotting=True)
                self.assertEqual(events, [])
                spectrogram = object()
                kwargs = {'clip_max': 3.0} if name.endswith('_clipped') else {}
                self.assertIs(module[name](spectrogram, **kwargs), fig)
                self.assertEqual(events, [('import', 'matplotlib'), ('backend', 'Agg'),
                                          ('import', 'matplotlib.pylab')])
                plt.subplots.assert_called_once_with(figsize=(10, 2))
                image_kwargs = dict(aspect='auto', origin='lower', interpolation='none')
                if kwargs:
                    image_kwargs.update(vmin=1e-6, vmax=3.0)
                ax.imshow.assert_called_once_with(spectrogram, **image_kwargs)
                plt.colorbar.assert_called_once_with(ax.imshow.return_value, ax=ax)
                fig.canvas.draw.assert_called_once_with()
                plt.close.assert_called_once_with()


if __name__ == '__main__':
    unittest.main()
