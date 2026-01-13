import json
import shutil
import stat
import subprocess
from contextlib import contextmanager
from pathlib import Path

import pytest
from hexbytes import HexBytes
from pystarport import ports
from pystarport.cluster import SUPERVISOR_CONFIG_FILE

from .network import Cronos, setup_custom_cronos
from .utils import (
    ADDRS,
    CONTRACTS,
    approve_proposal,
    deploy_contract,
    edit_ini_sections,
    send_transaction,
    wait_for_block,
    wait_for_port,
)

pytestmark = pytest.mark.upgrade


@pytest.fixture(scope="module")
def custom_cronos(tmp_path_factory):
    yield from setup_cronos_test(tmp_path_factory)


def init_cosmovisor(home):
    """
    build and setup cosmovisor directory structure in each node's home directory
    """
    cosmovisor = home / "cosmovisor"
    cosmovisor.mkdir()
    (cosmovisor / "upgrades").symlink_to("../../../upgrades")
    (cosmovisor / "genesis").symlink_to("./upgrades/genesis")


def post_init(path, base_port, config):
    """
    prepare cosmovisor for each node
    """
    chain_id = "cronos_777-1"
    data = path / chain_id
    cfg = json.loads((data / "config.json").read_text())
    for i, _ in enumerate(cfg["validators"]):
        home = data / f"node{i}"
        init_cosmovisor(home)

    edit_ini_sections(
        chain_id,
        data / SUPERVISOR_CONFIG_FILE,
        lambda i, _: {
            "command": f"cosmovisor run start --home %(here)s/node{i}",
            "environment": (
                "DAEMON_NAME=cronosd,"
                "DAEMON_SHUTDOWN_GRACE=1m,"
                "UNSAFE_SKIP_BACKUP=true,"
                f"DAEMON_HOME=%(here)s/node{i}"
            ),
        },
    )


def setup_cronos_test(tmp_path_factory):
    path = tmp_path_factory.mktemp("upgrade")
    port = 26200
    nix_name = "upgrade-test-package"
    cfg_name = "cosmovisor"
    configdir = Path(__file__).parent
    cmd = [
        "nix-build",
        configdir / f"configs/{nix_name}.nix",
    ]
    print(*cmd)
    subprocess.run(cmd, check=True)

    # copy the content so the new directory is writable.
    upgrades = path / "upgrades"
    shutil.copytree("./result", upgrades)
    mod = stat.S_IRWXU
    upgrades.chmod(mod)
    for d in upgrades.iterdir():
        d.chmod(mod)

    # init with genesis binary
    with contextmanager(setup_custom_cronos)(
        path,
        port,
        configdir / f"configs/{cfg_name}.jsonnet",
        post_init=post_init,
        chain_binary=str(upgrades / "genesis/bin/cronosd"),
    ) as cronos:
        yield cronos


def check_basic_tx(c):
    # check basic tx works
    wait_for_port(ports.evmrpc_port(c.base_port(0)))
    receipt = send_transaction(
        c.w3,
        {
            "to": ADDRS["community"],
            "value": 1000,
            "maxFeePerGas": 10000000000000,
            "maxPriorityFeePerGas": 10000,
        },
    )
    assert receipt.status == 1


def exec(c, tmp_path_factory):
    """
    - propose an upgrade and pass it
    - wait for it to happen
    - it should work transparently
    Starting from v1.4 as genesis, upgrade through v1.5 -> v1.6 -> v1.7
    """
    cli = c.cosmos_cli()
    base_port = c.base_port(0)
    w3 = c.w3

    def do_upgrade(plan_name, target):
        print(f"upgrade {plan_name} height: {target}")
        rsp = cli.submit_gov_proposal(
            "community",
            "software-upgrade",
            {
                "name": plan_name,
                "title": "upgrade test",
                "note": "ditto",
                "upgrade-height": target,
                "summary": "summary",
                "deposit": "10000basetcro",
            },
            broadcast_mode="sync",
        )
        assert rsp["code"] == 0, rsp["raw_log"]
        approve_proposal(
            c, rsp["events"], msg="/cosmos.upgrade.v1beta1.MsgSoftwareUpgrade"
        )

        # update cli chain binary
        c.chain_binary = (
            Path(c.chain_binary).parent.parent.parent / f"{plan_name}/bin/cronosd"
        )
        # block should pass the target height
        wait_for_block(c.cosmos_cli(), target + 2, timeout=480)
        wait_for_port(ports.rpc_port(base_port))
        return c.cosmos_cli()

    check_basic_tx(c)

    # deploy contracts before upgrade to test historical queries
    contract = deploy_contract(w3, CONTRACTS["TestERC20A"])
    old_height = w3.eth.block_number
    old_balance = w3.eth.get_balance(ADDRS["validator"], block_identifier=old_height)
    old_base_fee = w3.eth.get_block(old_height).baseFeePerGas
    old_erc20_balance = contract.caller(block_identifier=old_height).balanceOf(
        ADDRS["validator"]
    )
    print("old values", old_height, old_balance, old_base_fee)

    to = "0x2D5B6C193C39D2AECb4a99052074E6F325258a0f"
    receipt = send_transaction(w3, {"to": to, "value": 10, "gas": 21000})
    method = "debug_traceTransaction"
    params = [receipt["transactionHash"].hex(), {"tracer": "callTracer"}]
    tx_bf = w3.provider.make_request(method, params)

    cli = do_upgrade("v1.5", cli.block_height() + 15)
    check_basic_tx(c)

    # query json-rpc on older blocks should succeed
    assert old_balance == w3.eth.get_balance(
        ADDRS["validator"], block_identifier=old_height
    )
    assert old_base_fee == w3.eth.get_block(old_height).baseFeePerGas
    assert old_erc20_balance == contract.caller(block_identifier=old_height).balanceOf(
        ADDRS["validator"]
    )

    tx_af = w3.provider.make_request(method, params)
    assert tx_af.get("result") == tx_bf.get("result"), tx_af

    cli = do_upgrade("v1.6", cli.block_height() + 15)
    check_basic_tx(c)

    tx_af = w3.provider.make_request(method, params)
    assert tx_af.get("result") == tx_bf.get("result"), tx_af

    cli = do_upgrade("v1.7", cli.block_height() + 15)
    check_basic_tx(c)

    tx_af = w3.provider.make_request(method, params)
    assert tx_af.get("result") == tx_bf.get("result"), tx_af

    # check preinstall correctly installed
    historical_storage_address = "0x0000F90827F1C53a10cb7A02335B175320002935"
    expected_historical_storage_address_code = (
        "3373fffffffffffffffffffffffffffffffffffffffe14604657602036036042575f356001"
        "43038111604257611fff81430311604257611fff9006545f5260205ff35b5f5ffd5b5f3561"
        "1fff60014303065500"
    )
    historical_storage_address_code = w3.eth.get_code(historical_storage_address)
    assert historical_storage_address_code == HexBytes(
        expected_historical_storage_address_code
    )


def test_cosmovisor_upgrade(custom_cronos: Cronos, tmp_path_factory):
    exec(custom_cronos, tmp_path_factory)
